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
	"github.com/starcat-app/starcat-history-api/internal/telemetry"
	"github.com/starcat-app/starcat-history-api/internal/version"
)

const (
	defaultPort = "5014"
	// defaultGitHubMaxConcurrency 是全局出站闸门的默认并发上限。
	// 8 的取舍：并发冷启动（多个仓库的 README 同时首次被访问）时，单仓库分页会占 4 个
	// 槽位，8 允许两三个仓库同时推进；再高就容易踩 GitHub 的二级限流，反而让整池 token
	// 被禁几分钟。饱和时请求排队，超时后回退到缓存或 stale 数据。
	defaultGitHubMaxConcurrency = 8
	defaultRegistryDir          = "./data/history-registry"
	defaultStoreFile            = "./data/history.sqlite"
	// 全量 4000 万级仓库快照可能超过 2 GiB，默认上限保留到 16 GiB；
	// 实际 Fly 卷容量与上传窗口仍由运维侧单独控制。
	defaultMaxBundleBytes    = int64(16 << 30)
	defaultSnapshotRetention = 3
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
	OfficialMemoryCacheTTL time.Duration
	NegativeCacheTTL       time.Duration
	GitHubMaxConcurrency   int
	MaximumPoints          int
	MaxBundleBytes         int64
	SnapshotRetention      int
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
		Port:        kitenv.OrDefault("PORT", defaultPort),
		APIKeys:     apiKeys,
		PublishKeys: optionalListEnv("PUBLISH_KEYS"),
		// GITHUB_TOKENS 支持逗号分隔多 token（provider 轮询分摊限额）；
		// 单值场景仍兼容 GITHUB_TOKEN。
		GitHubToken:            firstNonEmptyEnv("GITHUB_TOKENS", "GITHUB_TOKEN"),
		GitHubEndpoint:         kitenv.OrDefault("GITHUB_API_ENDPOINT", "https://api.github.com"),
		StoreFile:              envOrDefault("STORE_FILE", defaultStoreFile),
		RegistryDir:            envOrDefault("REGISTRY_DIR", defaultRegistryDir),
		MetricsStoreFile:       envOrDefault("METRICS_STORE_FILE", "./data/history-metrics.db"),
		MetadataTTL:            kitenv.DurationSeconds("METADATA_TTL_SECONDS", 24*time.Hour),
		OfficialMemoryCacheTTL: kitenv.DurationSeconds("OFFICIAL_MEMORY_CACHE_TTL_SECONDS", handler.DefaultOfficialMemoryCacheTTL),
		NegativeCacheTTL:       kitenv.DurationSeconds("METADATA_NEGATIVE_CACHE_TTL_SECONDS", handler.DefaultNegativeMetadataCacheTTL),
		GitHubMaxConcurrency:   intEnv("GITHUB_MAX_CONCURRENCY", defaultGitHubMaxConcurrency),
		MaximumPoints:          intEnv("MAXIMUM_HISTORY_POINTS", series.DefaultMaximumPoints),
		MaxBundleBytes:         int64Env("MAX_BUNDLE_BYTES", defaultMaxBundleBytes),
		SnapshotRetention:      intEnv("SNAPSHOT_RETENTION", defaultSnapshotRetention),
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
	if opt.OfficialMemoryCacheTTL <= 0 {
		opt.OfficialMemoryCacheTTL = handler.DefaultOfficialMemoryCacheTTL
	}
	if opt.NegativeCacheTTL <= 0 {
		opt.NegativeCacheTTL = handler.DefaultNegativeMetadataCacheTTL
	}
	if opt.MaximumPoints <= 0 {
		opt.MaximumPoints = series.DefaultMaximumPoints
	}
	if opt.MaxBundleBytes <= 0 {
		opt.MaxBundleBytes = defaultMaxBundleBytes
	}
	if opt.SnapshotRetention < 2 {
		opt.SnapshotRetention = defaultSnapshotRetention
	}
	registry, err := serving.NewRegistry(opt.RegistryDir, opt.StoreFile, opt.SnapshotRetention)
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
	// 进程内计数：kitmetrics 记录按路由的耗时/状态码，这里记录回源次数与缓存命中，
	// 两者互补。用于压测取差值，也用于线上判断「慢」是缓存问题还是 GitHub 问题。
	serviceTelemetry := telemetry.NewRegistry()
	metadataProvider := provider.NewGitHubProvider(opt.GitHubEndpoint, opt.GitHubToken, nil).
		WithTelemetry(serviceTelemetry).
		// 头像按 URL 复用同一份 SQLite：它几乎不变，不该跟着元数据 TTL 每次重下。
		WithAvatarCache(registry).
		// 所有出站 GitHub 调用（metadata / 分页 / 头像）共用一道全局闸门。
		WithConcurrencyLimit(opt.GitHubMaxConcurrency)
	// 历史曲线与仓库 metadata 共用 GitHub client，但数据源明确切换到官方
	// stargazers/history；旧 repo_history_series 仅继续服务 /events 调试接口。
	historyHandler := handler.NewHistoryHandler(
		registry, metadataProvider, opt.MetadataTTL, opt.MaximumPoints,
		handler.WithStarHistoryProvider(metadataProvider),
		handler.WithOfficialMemoryCacheTTL(opt.OfficialMemoryCacheTTL),
		handler.WithNegativeCacheTTL(opt.NegativeCacheTTL),
		handler.WithTelemetry(serviceTelemetry),
	)
	auth := middleware.NewBearerAuth(opt.APIKeys)
	metricsHandler := kitmetrics.NewHandler(Name(), metrics.Store())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthzHandler)
	mux.Handle("GET /api/v1/ping", auth.Wrap(handler.HandlePing(Name(), version.Version)))
	// README 的 <img> 请求无法携带 API_KEYS；公开 embed 仅允许固定的 SVG 参数，
	// handler 内部仍会按 owner/repo 做 GitHub Public 校验和 Serving 历史门禁。
	mux.Handle("GET /embed/v1/repos/{owner}/{repo}/star-history.svg", http.HandlerFunc(historyHandler.HandleStarHistoryEmbed))
	// 曲线接口与 SVG embed 同为面向第三方的公开入口：只返回公开仓库的重建
	// 历史曲线，handler 内部仍有 GitHub Public 校验，因此不再要求 API key。
	mux.Handle("GET /api/v1/repos/{owner}/{repo}/star-history", http.HandlerFunc(historyHandler.HandleStarHistory))
	mux.Handle("GET /api/v1/repos/{owner}/{repo}/star-history/events", auth.Wrap(http.HandlerFunc(historyHandler.HandleStarHistoryEvents)))
	mux.Handle("GET /internal/stats", auth.Wrap(handler.HandleStats(registry)))
	mux.Handle("GET /internal/metrics/summary", auth.Wrap(http.HandlerFunc(metricsHandler.HandleSummary)))
	mux.Handle("GET /internal/metrics/timeseries", auth.Wrap(http.HandlerFunc(metricsHandler.HandleTimeseries)))
	mux.Handle("GET /internal/metrics/routes", auth.Wrap(http.HandlerFunc(metricsHandler.HandleRoutes)))
	mux.Handle("GET /internal/metrics/status-codes", auth.Wrap(http.HandlerFunc(metricsHandler.HandleStatusCodes)))
	// 进程内计数：GET 读快照，POST 归零（压测取单场景差值用，因此不能用 GET 清零）。
	mux.Handle("GET /internal/metrics/service", auth.Wrap(handler.HandleServiceMetrics(serviceTelemetry)))
	mux.Handle("POST /internal/metrics/service/reset", auth.Wrap(handler.HandleServiceMetricsReset(serviceTelemetry)))
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
		log.Printf("  GET /embed/v1/repos/{owner}/{repo}/star-history.svg")
		log.Printf("  GET /api/v1/repos/{owner}/{repo}/star-history")
		log.Printf("  GET /api/v1/repos/{owner}/{repo}/star-history/events")
		log.Printf("  GET /internal/stats")
		log.Printf("  GET /internal/metrics/service")
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

// firstNonEmptyEnv 依次返回第一个非空环境变量；全部为空时返回空串。
func firstNonEmptyEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
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
