package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/cache"
	"github.com/starcat-app/starcat-history-api/internal/model"
	"github.com/starcat-app/starcat-history-api/internal/provider"
	"github.com/starcat-app/starcat-history-api/internal/series"
	"github.com/starcat-app/starcat-history-api/internal/serving"
	"github.com/starcat-app/starcat-history-api/internal/telemetry"
)

var repositoryPathPart = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// HistoryStore 是查询 handler 所需的最小存储接口。
type HistoryStore interface {
	Series(context.Context, int64) (serving.RepositorySeries, error)
	Metadata(context.Context, int64) (serving.RepositoryMetadata, bool, error)
	MetadataByFullName(context.Context, string) (serving.RepositoryMetadata, bool, error)
	SaveMetadata(context.Context, serving.RepositoryMetadata) error
	Active(context.Context) (serving.ActiveState, error)
}

// GitHubHistoryCacheStore 是官方周数据的持久化缓存能力。它独立于旧的 GH Archive
// 序列，保证官方历史可用性不再受 repo_history_series 覆盖范围影响。
type GitHubHistoryCacheStore interface {
	GitHubStarHistoryCache(context.Context, string, string) (model.GitHubStarHistoryCache, bool, error)
	SaveGitHubStarHistoryCache(context.Context, string, string, model.GitHubStarHistoryCache) error
	TouchGitHubStarHistoryCache(context.Context, string, string, time.Time) error
}

// HistoryHandlerOption 配置官方历史数据源。
type HistoryHandlerOption func(*HistoryHandler)

// WithStarHistoryProvider 让查询链路使用 GitHub 官方 Star 历史接口。
func WithStarHistoryProvider(value provider.StarHistoryProvider) HistoryHandlerOption {
	return func(h *HistoryHandler) { h.starHistory = value }
}

// WithOfficialMemoryCacheTTL 设置官方历史 payload 的进程内缓存时长。
// 小于等于 0 的值会被忽略，避免调用方无意中关闭缓存并放大 SQLite 读取。
func WithOfficialMemoryCacheTTL(value time.Duration) HistoryHandlerOption {
	return func(h *HistoryHandler) {
		if value > 0 {
			h.officialMemoryCacheTTL = value
		}
	}
}

// WithTelemetry 注入进程内计数指标，用于观测缓存命中与回源次数。
func WithTelemetry(registry *telemetry.Registry) HistoryHandlerOption {
	return func(h *HistoryHandler) { h.telemetry = registry }
}

// HistoryHandler 生成客户端兼容响应；生产配置优先使用 GitHub 官方周历史，
// 旧压缩序列只保留给 /events 原始事件接口和未装配官方 provider 的兼容测试。
type HistoryHandler struct {
	store                  HistoryStore
	metadata               provider.MetadataProvider
	starHistory            provider.StarHistoryProvider
	historyCache           GitHubHistoryCacheStore
	memory                 *cache.LRU
	flights                *cache.Group
	metadataTTL            time.Duration
	officialMemoryCacheTTL time.Duration
	maximumPoints          int
	now                    func() time.Time
	// telemetry 可为 nil；所有计数都走 nil-safe 方法，单测无需构造。
	telemetry *telemetry.Registry
}

// NewHistoryHandler 创建查询 handler。
func NewHistoryHandler(store HistoryStore, metadata provider.MetadataProvider, metadataTTL time.Duration, maximumPoints int, options ...HistoryHandlerOption) *HistoryHandler {
	if metadataTTL <= 0 {
		metadataTTL = 24 * time.Hour
	}
	if maximumPoints <= 0 {
		maximumPoints = series.DefaultMaximumPoints
	}
	handler := &HistoryHandler{
		store: store, metadata: metadata, metadataTTL: metadataTTL,
		officialMemoryCacheTTL: DefaultOfficialMemoryCacheTTL, maximumPoints: maximumPoints,
		now: time.Now, memory: cache.NewLRU(512), flights: &cache.Group{},
	}
	for _, option := range options {
		if option != nil {
			option(handler)
		}
	}
	if persisted, ok := store.(GitHubHistoryCacheStore); ok {
		handler.historyCache = persisted
	}
	return handler
}

// HandleStarHistory 处理 GET /api/v1/repos/{owner}/{repo}/star-history。
//
// 可选 query `current_stars`：合法时直接校准，不访问 GitHub metadata；缺省时读取公开 metadata。
func (h *HistoryHandler) HandleStarHistory(w http.ResponseWriter, r *http.Request) {
	if h.starHistory != nil {
		h.handleOfficialStarHistory(w, r)
		return
	}
	owner, repo, repoID, ok := parseRepositoryIdentity(w, r)
	if !ok {
		return
	}
	historyRange := model.HistoryRange(strings.TrimSpace(r.URL.Query().Get("range")))
	if historyRange == "" {
		historyRange = model.HistoryRangeOneYear
	}
	if !historyRange.Valid() {
		writeError(w, http.StatusBadRequest, "INVALID_RANGE", "Unsupported star history range.", nil)
		return
	}
	currentStars, provided, err := parseOptionalCurrentStars(r.URL.Query().Get("current_stars"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CURRENT_STARS", err.Error(), nil)
		return
	}

	storedSeries, events, ok := h.loadDecodedSeries(w, r, repoID)
	if !ok {
		return
	}

	fullName := owner + "/" + repo
	if provided {
		// 调用方已提供校准锚点（常见于自有客户端缓存），跳过 GitHub 以节省额度。
	} else {
		metadata, ok := h.requirePublicMetadata(w, r, repoID, owner, repo)
		if !ok {
			return
		}
		currentStars = metadata.CurrentStars
		fullName = metadata.FullName
	}

	points, err := series.Normalize(events, currentStars)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "NORMALIZE_FAILED", "Unable to normalize star history.", nil)
		return
	}
	now := h.now().UTC()
	points, err = series.SelectRange(points, historyRange, now, h.maximumPoints)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DOWNSAMPLE_FAILED", "Unable to downsample star history.", nil)
		return
	}
	active, ok := h.requireActive(w, r)
	if !ok {
		return
	}
	generatedAt := active.GeneratedAt
	if generatedAt.IsZero() {
		generatedAt = now
	}
	etag := historyETag(repoID, historyRange, currentStars, storedSeries.SeriesChecksum, active)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	response := model.HistoryResponse{
		RepoID: repoID, FullName: fullName, CurrentStars: currentStars,
		Range: historyRange, CoverageStart: series.TimeFromDay(storedSeries.CoverageStartDay).Format("2006-01-02"),
		GeneratedAt: generatedAt.UTC(), Points: points, ModelVersion: active.ModelVersion,
		ActiveWatermark: active.ActiveWatermark,
	}
	writeJSON(w, http.StatusOK, response)
}

// HandleStarHistoryEvents 处理 GET /api/v1/repos/{owner}/{repo}/star-history/events。
//
// Starcat 专用：只返回日级 WatchEvent 计数，不访问 GitHub，也不做 Star 曲线校准。
func (h *HistoryHandler) HandleStarHistoryEvents(w http.ResponseWriter, r *http.Request) {
	owner, repo, repoID, ok := parseRepositoryIdentity(w, r)
	if !ok {
		return
	}
	if repoID <= 0 {
		writeError(w, http.StatusBadRequest, "INVALID_REPOSITORY", "repo_id is required for events.", nil)
		return
	}

	storedSeries, events, ok := h.loadDecodedSeries(w, r, repoID)
	if !ok {
		return
	}
	active, ok := h.requireActive(w, r)
	if !ok {
		return
	}
	now := h.now().UTC()
	generatedAt := active.GeneratedAt
	if generatedAt.IsZero() {
		generatedAt = now
	}

	payload := make([]model.HistoryEvent, 0, len(events))
	for _, event := range events {
		if event.Count > uint64(^uint(0)>>1) {
			writeError(w, http.StatusInternalServerError, "CORRUPT_HISTORY", "Stored star history is corrupt.", nil)
			return
		}
		payload = append(payload, model.HistoryEvent{
			Date:  series.TimeFromDay(event.Day).Format("2006-01-02"),
			Count: int(event.Count),
		})
	}

	etag := eventsETag(repoID, storedSeries.SeriesChecksum, active)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	response := model.HistoryEventsResponse{
		RepoID:          repoID,
		FullName:        owner + "/" + repo,
		CoverageStart:   series.TimeFromDay(storedSeries.CoverageStartDay).Format("2006-01-02"),
		CoverageEnd:     series.TimeFromDay(storedSeries.CoverageEndDay).Format("2006-01-02"),
		EventTotal:      storedSeries.EventTotal,
		GeneratedAt:     generatedAt.UTC(),
		Events:          payload,
		ModelVersion:    active.ModelVersion,
		ActiveWatermark: active.ActiveWatermark,
	}
	writeJSON(w, http.StatusOK, response)
}

func parseRepositoryIdentity(w http.ResponseWriter, r *http.Request) (owner, repo string, repoID int64, ok bool) {
	owner, repo = strings.TrimSpace(r.PathValue("owner")), strings.TrimSpace(r.PathValue("repo"))
	if !repositoryPathPart.MatchString(owner) || !repositoryPathPart.MatchString(repo) {
		writeError(w, http.StatusBadRequest, "INVALID_REPOSITORY", "Invalid repository path.", nil)
		return "", "", 0, false
	}
	rawRepoID := strings.TrimSpace(r.URL.Query().Get("repo_id"))
	if rawRepoID == "" {
		return owner, repo, 0, true
	}
	repoID, err := strconv.ParseInt(rawRepoID, 10, 64)
	if err != nil || repoID <= 0 {
		writeError(w, http.StatusBadRequest, "INVALID_REPOSITORY", "repo_id must be a positive integer.", nil)
		return "", "", 0, false
	}
	return owner, repo, repoID, true
}

// parseOptionalCurrentStars 解析可选校准锚点。
// 未传或空串 → provided=false；显式非法值 → error，避免静默回退打 GitHub。
func parseOptionalCurrentStars(raw string) (value int, provided bool, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, false, nil
	}
	parsed, parseErr := strconv.Atoi(trimmed)
	if parseErr != nil || parsed < 0 {
		return 0, false, fmt.Errorf("current_stars must be a non-negative integer.")
	}
	return parsed, true, nil
}

func (h *HistoryHandler) loadDecodedSeries(w http.ResponseWriter, r *http.Request, repoID int64) (serving.RepositorySeries, []series.DayCount, bool) {
	storedSeries, err := h.store.Series(r.Context(), repoID)
	if errors.Is(err, serving.ErrSeriesNotFound) {
		writeError(w, http.StatusNotFound, "HISTORY_NOT_FOUND", "Star history is not available for this repository.", nil)
		return serving.RepositorySeries{}, nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "Unable to read star history.", nil)
		return serving.RepositorySeries{}, nil, false
	}
	events, err := series.Decode(storedSeries.Encoding, storedSeries.Series, storedSeries.SeriesChecksum, storedSeries.PointCount)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CORRUPT_HISTORY", "Stored star history is corrupt.", nil)
		return serving.RepositorySeries{}, nil, false
	}
	return storedSeries, events, true
}

func (h *HistoryHandler) requirePublicMetadata(w http.ResponseWriter, r *http.Request, repoID int64, owner, repo string) (serving.RepositoryMetadata, bool) {
	metadata, err := h.resolveMetadata(r.Context(), repoID, owner, repo)
	if errors.Is(err, provider.ErrNotFound) {
		writeError(w, http.StatusNotFound, "REPOSITORY_NOT_FOUND", "Repository was not found.", nil)
		return serving.RepositoryMetadata{}, false
	}
	if errors.Is(err, provider.ErrRateLimited) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "GITHUB_RATE_LIMITED", "GitHub metadata is temporarily unavailable.", nil)
		return serving.RepositoryMetadata{}, false
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "GITHUB_ERROR", "Unable to refresh repository metadata.", nil)
		return serving.RepositoryMetadata{}, false
	}
	if repoID > 0 && metadata.RepoID != repoID {
		writeError(w, http.StatusConflict, "REPOSITORY_ID_MISMATCH", "Repository ID does not match the requested path.", nil)
		return serving.RepositoryMetadata{}, false
	}
	if metadata.Visibility != "public" {
		writeError(w, http.StatusUnprocessableEntity, "PRIVATE_REPOSITORY", "Private and internal repositories are not served.", nil)
		return serving.RepositoryMetadata{}, false
	}
	return metadata, true
}

func (h *HistoryHandler) requireActive(w http.ResponseWriter, r *http.Request) (serving.ActiveState, bool) {
	active, err := h.store.Active(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "Unable to read active history version.", nil)
		return serving.ActiveState{}, false
	}
	return active, true
}

func (h *HistoryHandler) resolveMetadata(ctx context.Context, repoID int64, owner, repo string) (serving.RepositoryMetadata, error) {
	cached, found, err := h.store.Metadata(ctx, repoID)
	if err != nil {
		return serving.RepositoryMetadata{}, err
	}
	if found && h.now().UTC().Sub(cached.CheckedAt) < h.metadataTTL {
		h.telemetry.MetadataCacheHit()
		return cached, nil
	}
	if h.metadata == nil {
		if found {
			return cached, nil
		}
		return serving.RepositoryMetadata{}, fmt.Errorf("metadata provider is not configured")
	}
	h.telemetry.MetadataCacheMiss()
	fresh, err := h.metadata.Fetch(ctx, owner, repo)
	if err != nil {
		// GitHub 临时失败时允许使用已经验证过的旧公开元数据，避免外部依赖拖垮历史接口。
		if found && cached.Visibility == "public" {
			return cached, nil
		}
		return serving.RepositoryMetadata{}, err
	}
	if repoID > 0 && fresh.RepoID != repoID {
		return serving.RepositoryMetadata{}, provider.ErrNotFound
	}
	if err := h.store.SaveMetadata(ctx, fresh); err != nil {
		return serving.RepositoryMetadata{}, err
	}
	return fresh, nil
}

// resolvePublicMetadata 按 owner/repo 解析公开嵌入所需的不可变 repo ID。
//
// README 图片请求不可能携带 Starcat API key，因此先使用完整仓库名缓存，再在 TTL
// 到期时调用 GitHub 官方 metadata。缓存命中仍需保持 Public 门禁，避免仓库变私有后
// 继续把历史曲线暴露给公开图片地址。
func (h *HistoryHandler) resolvePublicMetadata(ctx context.Context, owner, repo string) (serving.RepositoryMetadata, error) {
	fullName := owner + "/" + repo
	cached, found, err := h.store.MetadataByFullName(ctx, fullName)
	if err != nil {
		return serving.RepositoryMetadata{}, err
	}
	if found && h.now().UTC().Sub(cached.CheckedAt) < h.metadataTTL {
		h.telemetry.MetadataCacheHit()
		return cached, nil
	}
	if h.metadata == nil {
		if found && cached.Visibility == "public" {
			return cached, nil
		}
		return serving.RepositoryMetadata{}, fmt.Errorf("metadata provider is not configured")
	}
	h.telemetry.MetadataCacheMiss()
	fresh, err := h.metadata.Fetch(ctx, owner, repo)
	if err != nil {
		if found && cached.Visibility == "public" {
			return cached, nil
		}
		return serving.RepositoryMetadata{}, err
	}
	if fresh.Visibility != "public" {
		return fresh, nil
	}
	if err := h.store.SaveMetadata(ctx, fresh); err != nil {
		return serving.RepositoryMetadata{}, err
	}
	return fresh, nil
}

func historyETag(repoID int64, historyRange model.HistoryRange, currentStars int, checksum string, active serving.ActiveState) string {
	value := fmt.Sprintf("%d|%s|%d|%s|%s|%s", repoID, historyRange, currentStars, checksum, active.ModelVersion, active.ActiveWatermark)
	sum := sha256.Sum256([]byte(value))
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

func eventsETag(repoID int64, checksum string, active serving.ActiveState) string {
	value := fmt.Sprintf("events|%d|%s|%s|%s", repoID, checksum, active.ModelVersion, active.ActiveWatermark)
	sum := sha256.Sum256([]byte(value))
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}
