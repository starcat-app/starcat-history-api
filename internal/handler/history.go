package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
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
	NegativeMetadata(context.Context, string) (string, time.Time, bool, error)
	SaveNegativeMetadata(context.Context, string, string, time.Time) error
	ClearNegativeMetadata(context.Context, string) error
	Active(context.Context) (serving.ActiveState, error)
}

// GitHubHistoryCacheStore 是官方周数据的持久化缓存能力。它独立于旧的 GH Archive
// 序列，保证官方历史可用性不再受 repo_history_series 覆盖范围影响。
type GitHubHistoryCacheStore interface {
	GitHubStarHistoryCache(context.Context, string, string) (model.GitHubStarHistoryCache, bool, error)
	SaveGitHubStarHistoryCache(context.Context, string, string, model.GitHubStarHistoryCache) error
	TouchGitHubStarHistoryCache(context.Context, string, string, time.Time) error
}

// DefaultNegativeMetadataCacheTTL 是「不可用仓库」的默认负缓存时长。
//
// 取 1 小时是两组需求的交点：一方面公开入口会被任意 owner/repo 扫，没有负缓存就是
// 每次一次 GitHub 调用；另一方面仓库从私有转公开、或误删后重建，最多只应等这么久
// 就能重新被收录。
const DefaultNegativeMetadataCacheTTL = time.Hour

// 负缓存原因，仅用于排查（存进 repository_metadata_negative.reason）。
const (
	metadataNegativeNotFound  = "not_found"
	metadataNegativeNotPublic = "not_public"
)

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

// WithNegativeCacheTTL 设置「不可用仓库」（404 / 非公开）的负缓存时长。
// 小于等于 0 的值会被忽略：关掉负缓存等于把公开入口直接暴露给任意 owner/repo 刷量。
func WithNegativeCacheTTL(value time.Duration) HistoryHandlerOption {
	return func(h *HistoryHandler) {
		if value > 0 {
			h.negativeCacheTTL = value
		}
	}
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
	negativeCacheTTL       time.Duration
	maximumPoints          int
	now                    func() time.Time
	// telemetry 可为 nil；所有计数都走 nil-safe 方法，单测无需构造。
	telemetry *telemetry.Registry
	// backoff 记录每个仓库下一次允许回源的时间，避免故障期把重试变成放大器。
	backoff *refreshBackoff
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
		negativeCacheTTL: DefaultNegativeMetadataCacheTTL,
		now:              time.Now, memory: cache.NewLRU(512), flights: &cache.Group{},
		backoff: newRefreshBackoff(),
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
	if ifNoneMatch(r.Header.Get("If-None-Match"), etag) {
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
	if ifNoneMatch(r.Header.Get("If-None-Match"), etag) {
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
	metadata, err := h.resolveMetadataCached(r.Context(), repoID, owner, repo)
	if errors.Is(err, provider.ErrNotFound) {
		writeError(w, http.StatusNotFound, "REPOSITORY_NOT_FOUND", "Repository was not found.", nil)
		return serving.RepositoryMetadata{}, false
	}
	if isUpstreamBusy(err) {
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

// lookupMetadataCache 先按 repo_id、再按 full name 查公开元数据缓存。
//
// 为什么必须回退 full name：曲线接口的 repo_id 是可选 query，第三方不传时 repoID 为 0，
// 而缓存行里的 repo_id 永远是真实 GitHub ID（>0），`WHERE repo_id = 0` 永不命中 ——
// 缓存写进去了却永远读不到，于是每个请求都回源 GitHub（线上实测 8.5s/次）。
func (h *HistoryHandler) lookupMetadataCache(ctx context.Context, repoID int64, owner, repo string) (serving.RepositoryMetadata, bool, error) {
	if repoID > 0 {
		value, found, err := h.store.Metadata(ctx, repoID)
		if err != nil {
			return serving.RepositoryMetadata{}, false, err
		}
		if found {
			return value, true, nil
		}
	}
	value, found, err := h.store.MetadataByFullName(ctx, owner+"/"+repo)
	if err != nil {
		return serving.RepositoryMetadata{}, false, err
	}
	if !found {
		return serving.RepositoryMetadata{}, false, nil
	}
	// 调用方指名了 repo_id 时，按名字命中的行必须与之一致；不一致宁可当未命中，
	// 交给下面的回源重新确认（仓库改名/转移后 full name 可能指向另一条记录）。
	if repoID > 0 && value.RepoID != repoID {
		return serving.RepositoryMetadata{}, false, nil
	}
	return value, true, nil
}

// resolveMetadataCached 解析仓库元数据，两条查询路径共用：
// 内存/持久缓存 → 负缓存 → singleflight 回源。
//
// 关于 Public 门禁：README 图片请求不可能携带 API key，所以这里既是缓存入口也是
// 唯一的可见性校验点。"不可用"（404 或非公开）会被写进负缓存，避免公开入口被
// 反复抓取不存在的仓库时无上限消耗 GitHub 额度。
func (h *HistoryHandler) resolveMetadataCached(ctx context.Context, repoID int64, owner, repo string) (serving.RepositoryMetadata, error) {
	fullName := owner + "/" + repo
	cached, found, err := h.lookupMetadataCache(ctx, repoID, owner, repo)
	if err != nil {
		return serving.RepositoryMetadata{}, err
	}
	now := h.now().UTC()
	if found && now.Sub(cached.CheckedAt) < h.metadataTTL {
		h.telemetry.MetadataCacheHit()
		return cached, nil
	}
	if reason, checkedAt, ok := h.negativeMetadata(ctx, fullName); ok && now.Sub(checkedAt) < h.negativeCacheTTL {
		h.telemetry.MetadataNegativeHit()
		if reason == metadataNegativeNotFound {
			return serving.RepositoryMetadata{}, provider.ErrNotFound
		}
		// 只知道"不能对外提供"，不知道它的 repo_id；调用方指名了就把那个 id 带回去，
		// 让两条路径各自走到既有的「非公开」分支（SVG 404 / 曲线 422）。
		return serving.RepositoryMetadata{
			RepoID: repoID, FullName: fullName, Visibility: "private", CheckedAt: checkedAt,
		}, nil
	}
	if h.metadata == nil {
		if found {
			return cached, nil
		}
		return serving.RepositoryMetadata{}, fmt.Errorf("metadata provider is not configured")
	}
	h.telemetry.MetadataCacheMiss()
	// 同一仓库的并发冷启动共享一次回源。没有它，N 个并发请求就是 N 次 GitHub 调用 ——
	// 而"一个仓库的 README 首次被看到"恰好就是这种突发形态。
	value, err := h.flights.Do(ctx, metadataFlightKey(fullName), func() (any, error) {
		return h.refreshMetadata(ctx, repoID, owner, repo, fullName, cached, found)
	})
	if err != nil {
		return serving.RepositoryMetadata{}, err
	}
	return value.(serving.RepositoryMetadata), nil
}

// refreshMetadata 回源 GitHub 并把结果落到正/负缓存。
func (h *HistoryHandler) refreshMetadata(
	ctx context.Context,
	repoID int64,
	owner, repo, fullName string,
	cached serving.RepositoryMetadata,
	found bool,
) (serving.RepositoryMetadata, error) {
	fresh, err := h.metadata.Fetch(ctx, owner, repo)
	if err != nil {
		if errors.Is(err, provider.ErrNotFound) {
			// 不存在的仓库必须留下负缓存：公开入口可以被任意 owner/repo 刷。
			h.recordNegativeMetadata(ctx, fullName, metadataNegativeNotFound)
			return serving.RepositoryMetadata{}, err
		}
		// GitHub 临时失败时允许使用已经验证过的旧公开元数据，避免外部依赖拖垮历史接口。
		if found && cached.Visibility == "public" {
			return cached, nil
		}
		return serving.RepositoryMetadata{}, err
	}
	if repoID > 0 && fresh.RepoID != repoID {
		return serving.RepositoryMetadata{}, provider.ErrNotFound
	}
	if fresh.Visibility != "public" {
		// 仓库存在但不可对外提供：记负缓存后原样返回，由调用方转成 404/422。
		// 不再落 repository_metadata —— 那张表的口径是"可用公开仓库"。
		h.recordNegativeMetadata(ctx, fullName, metadataNegativeNotPublic)
		return fresh, nil
	}
	if err := h.store.SaveMetadata(ctx, fresh); err != nil {
		return serving.RepositoryMetadata{}, err
	}
	// 曾经被判不可用、现在已公开：清掉负缓存，否则会在 TTL 内继续被拒。
	h.clearNegativeMetadata(ctx, fullName)
	return fresh, nil
}

// negativeMetadata 读负缓存。缓存故障只降级为"没有负缓存"，不影响请求结果。
func (h *HistoryHandler) negativeMetadata(ctx context.Context, fullName string) (string, time.Time, bool) {
	reason, checkedAt, found, err := h.store.NegativeMetadata(ctx, fullName)
	if err != nil || !found {
		return "", time.Time{}, false
	}
	return reason, checkedAt, true
}

func (h *HistoryHandler) recordNegativeMetadata(ctx context.Context, fullName, reason string) {
	if err := h.store.SaveNegativeMetadata(ctx, fullName, reason, h.now().UTC()); err != nil {
		log.Printf("[handler] save negative metadata cache for %s: %v", fullName, err)
	}
}

func (h *HistoryHandler) clearNegativeMetadata(ctx context.Context, fullName string) {
	if err := h.store.ClearNegativeMetadata(ctx, fullName); err != nil {
		log.Printf("[handler] clear negative metadata cache for %s: %v", fullName, err)
	}
}

func metadataFlightKey(fullName string) string {
	return "metadata|" + strings.ToLower(strings.TrimSpace(fullName))
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
