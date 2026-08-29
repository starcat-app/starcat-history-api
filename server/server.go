// Package server 导出 history-api 的可装配 HTTP 服务。
//
// 单仓部署走 cmd/server；聚合部署（starcat-api）import 本包并挂到统一网关。
package server

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	kitenv "github.com/starcat-app/starcat-api-kit/env"
	kitmetrics "github.com/starcat-app/starcat-api-kit/metrics"
	"github.com/starcat-app/starcat-history-api/internal/handler"
	"github.com/starcat-app/starcat-history-api/internal/middleware"
	"github.com/starcat-app/starcat-history-api/internal/provider"
	"github.com/starcat-app/starcat-history-api/internal/series"
	"github.com/starcat-app/starcat-history-api/internal/serving"
	"github.com/starcat-app/starcat-history-api/internal/version"
)

const (
	defaultPort        = "5014"
	defaultRegistryDir = "./data/history-registry"
	defaultStoreFile   = "./data/history.sqlite"
	// 全量 4000 万级仓库快照可能超过 2 GiB，默认上限保留到 16 GiB；
	// 实际 Fly 卷容量与上传窗口仍由运维侧单独控制。
	defaultMaxBundleBytes = int64(16 << 30)
)

// Options 控制 History 服务装配。
type Options struct {
	Port                   string
	APIKeys                []string
	PublishKeys            []string
	GitHubToken            string
	GitHubEndpoint         string
	StoreFile              string
	RegistryDir            string
	MetricsStoreFile       string
	MetadataTTL            time.Duration
	MaximumPoints          int
	MaxBundleBytes         int64
	SkipListenLogEndpoints bool
}

// Service 是已装配的 History HTTP 服务。
type Service struct {
	opts      Options
	handler   http.Handler
	registry  *serving.Registry
	metrics   *kitmetrics.Collector
	closeOnce sync.Once
}

func Name() string        { return version.Service }
func DefaultPort() string { return defaultPort }

// FromEnv 从独立服务或聚合服务注入的环境变量装配。
func FromEnv() (*Service, error) {
	apiKeys, err := kitenv.RequiredCSV("API_KEYS")
	if err != nil {
		return nil, err
	}
	return New(Options{
		Port:             kitenv.OrDefault("PORT", defaultPort),
		APIKeys:          apiKeys,
		PublishKeys:      optionalListEnv("PUBLISH_KEYS"),
		GitHubToken:      strings.TrimSpace(os.Getenv("GITHUB_TOKEN")),
		GitHubEndpoint:   kitenv.OrDefault("GITHUB_API_ENDPOINT", "https://api.github.com"),
		StoreFile:        envOrDefault("STORE_FILE", defaultStoreFile),
		RegistryDir:      envOrDefault("REGISTRY_DIR", defaultRegistryDir),
		MetricsStoreFile: envOrDefault("METRICS_STORE_FILE", "./data/history-metrics.db"),
		MetadataTTL:      kitenv.DurationSeconds("METADATA_TTL_SECONDS", 24*time.Hour),
		MaximumPoints:    intEnv("MAXIMUM_HISTORY_POINTS", series.DefaultMaximumPoints),
		MaxBundleBytes:   int64Env("MAX_BUNDLE_BYTES", defaultMaxBundleBytes),
	})
}

// New 装配所有路由和生命周期依赖。
func New(opt Options) (*Service, error) {
	if strings.TrimSpace(opt.Port) == "" {
		opt.Port = defaultPort
	}
	if len(opt.APIKeys) == 0 {
		return nil, fmt.Errorf("APIKeys is required")
	}
	if strings.TrimSpace(opt.StoreFile) == "" {
		opt.StoreFile = defaultStoreFile
	}
	if strings.TrimSpace(opt.RegistryDir) == "" {
		opt.RegistryDir = defaultRegistryDir
	}
	if strings.TrimSpace(opt.MetricsStoreFile) == "" {
		opt.MetricsStoreFile = ":memory:"
	}
	if opt.MetadataTTL <= 0 {
		opt.MetadataTTL = 24 * time.Hour
	}
	if opt.MaximumPoints <= 0 {
		opt.MaximumPoints = series.DefaultMaximumPoints
	}
	if opt.MaxBundleBytes <= 0 {
		opt.MaxBundleBytes = defaultMaxBundleBytes
	}
	registry, err := serving.NewRegistry(opt.RegistryDir, opt.StoreFile)
	if err != nil {
		return nil, fmt.Errorf("initialize history registry: %w", err)
	}
	metricsStore, err := kitmetrics.OpenSQLite(opt.MetricsStoreFile)
	if err != nil {
		registry.Close()
		return nil, fmt.Errorf("initialize metrics SQLite: %w", err)
	}
	metrics, err := kitmetrics.NewCollector(kitmetrics.Config{Service: Name(), Store: metricsStore})
	if err != nil {
		metricsStore.Close()
		registry.Close()
		return nil, fmt.Errorf("initialize metrics collector: %w", err)
	}
	metadataProvider := provider.NewGitHubProvider(opt.GitHubEndpoint, opt.GitHubToken, nil)
	historyHandler := handler.NewHistoryHandler(registry, metadataProvider, opt.MetadataTTL, opt.MaximumPoints)
	auth := middleware.NewBearerAuth(opt.APIKeys)
	metricsHandler := kitmetrics.NewHandler(Name(), metrics.Store())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthzHandler)
	mux.Handle("GET /api/v1/ping", auth.Wrap(handler.HandlePing(Name(), version.Version)))
	mux.Handle("GET /api/v1/repos/{owner}/{repo}/star-history", auth.Wrap(http.HandlerFunc(historyHandler.HandleStarHistory)))
	mux.Handle("GET /internal/stats", auth.Wrap(handler.HandleStats(registry)))
	mux.Handle("GET /internal/metrics/summary", auth.Wrap(http.HandlerFunc(metricsHandler.HandleSummary)))
	mux.Handle("GET /internal/metrics/timeseries", auth.Wrap(http.HandlerFunc(metricsHandler.HandleTimeseries)))
	mux.Handle("GET /internal/metrics/routes", auth.Wrap(http.HandlerFunc(metricsHandler.HandleRoutes)))
	mux.Handle("GET /internal/metrics/status-codes", auth.Wrap(http.HandlerFunc(metricsHandler.HandleStatusCodes)))
	if len(opt.PublishKeys) > 0 {
		publishAuth := middleware.NewBearerAuth(opt.PublishKeys)
		publishHandler := handler.NewPublishHandler(registry, opt.MaxBundleBytes)
		mux.Handle("POST /internal/v1/history-snapshots/{model_version}", publishAuth.Wrap(http.HandlerFunc(publishHandler.HandleSnapshotUpload)))
		mux.Handle("POST /internal/v1/history-snapshots/{model_version}/activate", publishAuth.Wrap(http.HandlerFunc(publishHandler.HandleSnapshotActivate)))
		mux.Handle("POST /internal/v1/history-deltas/{delta_id}", publishAuth.Wrap(http.HandlerFunc(publishHandler.HandleDeltaUpload)))
		mux.Handle("GET /internal/v1/history-active", publishAuth.Wrap(http.HandlerFunc(publishHandler.HandleActive)))
	}
	if !opt.SkipListenLogEndpoints {
		log.Printf("starcat-history-api %s endpoints ready", version.Version)
		log.Printf("  GET /api/v1/repos/{owner}/{repo}/star-history")
		log.Printf("  GET /internal/stats")
		if len(opt.PublishKeys) > 0 {
			log.Printf("  POST /internal/v1/history-snapshots/{model_version}")
			log.Printf("  POST /internal/v1/history-deltas/{delta_id}")
		}
	}
	return &Service{opts: opt, handler: metrics.Wrap(middleware.CORS(mux)), registry: registry, metrics: metrics}, nil
}

func (s *Service) Handler() http.Handler { return s.handler }
func (s *Service) Addr() string          { return ":" + s.opts.Port }

func (s *Service) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		if s.registry != nil {
			closeErr = s.registry.Close()
		}
		if s.metrics != nil {
			if err := s.metrics.Close(); closeErr == nil {
				closeErr = err
			}
		}
	})
	return closeErr
}

func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func optionalListEnv(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	var result []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func intEnv(key string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func int64Env(key string, fallback int64) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(key)), 10, 64)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
