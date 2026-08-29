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

	"github.com/starcat-app/starcat-history-api/internal/model"
	"github.com/starcat-app/starcat-history-api/internal/provider"
	"github.com/starcat-app/starcat-history-api/internal/series"
	"github.com/starcat-app/starcat-history-api/internal/serving"
)

var repositoryPathPart = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// HistoryStore 是查询 handler 所需的最小存储接口。
type HistoryStore interface {
	Series(context.Context, int64) (serving.RepositorySeries, error)
	Metadata(context.Context, int64) (serving.RepositoryMetadata, bool, error)
	SaveMetadata(context.Context, serving.RepositoryMetadata) error
	Active(context.Context) (serving.ActiveState, error)
}

// HistoryHandler 从预计算压缩序列生成客户端兼容响应。
type HistoryHandler struct {
	store         HistoryStore
	metadata      provider.MetadataProvider
	metadataTTL   time.Duration
	maximumPoints int
	now           func() time.Time
}

// NewHistoryHandler 创建查询 handler。
func NewHistoryHandler(store HistoryStore, metadata provider.MetadataProvider, metadataTTL time.Duration, maximumPoints int) *HistoryHandler {
	if metadataTTL <= 0 {
		metadataTTL = 24 * time.Hour
	}
	if maximumPoints <= 0 {
		maximumPoints = series.DefaultMaximumPoints
	}
	return &HistoryHandler{store: store, metadata: metadata, metadataTTL: metadataTTL, maximumPoints: maximumPoints, now: time.Now}
}

// HandleStarHistory 处理 GET /api/v1/repos/{owner}/{repo}/star-history。
//
// 可选 query `current_stars`：合法时直接校准，不访问 GitHub；缺省时回退 GitHub metadata。
func (h *HistoryHandler) HandleStarHistory(w http.ResponseWriter, r *http.Request) {
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
	repoID, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("repo_id")), 10, 64)
	if err != nil || repoID <= 0 {
		writeError(w, http.StatusBadRequest, "INVALID_REPOSITORY", "repo_id is required.", nil)
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
	if metadata.RepoID != repoID {
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
		return cached, nil
	}
	if h.metadata == nil {
		if found {
			return cached, nil
		}
		return serving.RepositoryMetadata{}, fmt.Errorf("metadata provider is not configured")
	}
	fresh, err := h.metadata.Fetch(ctx, owner, repo)
	if err != nil {
		// GitHub 临时失败时允许使用已经验证过的旧公开元数据，避免外部依赖拖垮历史接口。
		if found && cached.Visibility == "public" {
			return cached, nil
		}
		return serving.RepositoryMetadata{}, err
	}
	if fresh.RepoID != repoID {
		return serving.RepositoryMetadata{}, provider.ErrNotFound
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
