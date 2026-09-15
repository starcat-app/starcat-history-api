// Package handler 负责把 GitHub 官方周级 Star 历史转换为 History API 与 SVG 响应。
//
// 这里的原始周数据是唯一事实来源；日点、累计曲线和 SVG 都是可重建结果。因此
// SQLite 只保存官方 payload/ETag/时间戳，内存层保存短 TTL 副本，避免高频 README
// 请求重复访问 GitHub，同时保留跨进程重启后的缓存收益。
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/card"
	"github.com/starcat-app/starcat-history-api/internal/model"
	"github.com/starcat-app/starcat-history-api/internal/provider"
	"github.com/starcat-app/starcat-history-api/internal/series"
	"github.com/starcat-app/starcat-history-api/internal/serving"
)

const (
	// DefaultOfficialMemoryCacheTTL 是官方历史数据在进程内缓存的默认时长。
	// 外部 SVG 和 SQLite 仍有更长缓存，因此这里主要控制高频请求的 DB 读取频率。
	DefaultOfficialMemoryCacheTTL = 30 * time.Minute
	officialHistoryCacheTTL       = 24 * time.Hour
	fullHistoryRefreshAfter       = 7 * 24 * time.Hour
	officialHistoryPerPage        = 30
	officialHistoryMaxPages       = 100
	officialHistoryModelName      = "github-history-v1"
)

func (h *HistoryHandler) handleOfficialStarHistory(w http.ResponseWriter, r *http.Request) {
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
	fullName := owner + "/" + repo
	if !provided {
		metadata, ok := h.requirePublicMetadata(w, r, repoID, owner, repo)
		if !ok {
			return
		}
		currentStars, fullName, repoID = metadata.CurrentStars, metadata.FullName, metadata.RepoID
	}

	cached, err := h.loadOfficialHistory(r.Context(), owner, repo)
	if err != nil {
		h.writeOfficialHistoryError(w, err)
		return
	}
	events, err := series.OfficialEvents(cached.Weeks)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CORRUPT_HISTORY", "Cached GitHub star history is invalid.", nil)
		return
	}
	points, err := series.NormalizeOfficial(events, currentStars)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "NORMALIZE_FAILED", "Unable to normalize star history.", nil)
		return
	}
	points, err = series.SelectRange(points, historyRange, h.now().UTC(), h.maximumPoints)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DOWNSAMPLE_FAILED", "Unable to downsample star history.", nil)
		return
	}
	if len(points) == 0 {
		writeError(w, http.StatusNotFound, "HISTORY_NOT_FOUND", "Star history is not available for this repository.", nil)
		return
	}
	etag := officialHistoryETag(owner, repo, repoID, currentStars, historyRange, cached)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	generatedAt := cached.FetchedAt
	if generatedAt.IsZero() {
		generatedAt = h.now().UTC()
	}
	response := model.HistoryResponse{
		RepoID: repoID, FullName: fullName, CurrentStars: currentStars,
		Range: historyRange, CoverageStart: points[0].Date,
		GeneratedAt: generatedAt.UTC(), Points: points, ModelVersion: officialHistoryModelName,
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *HistoryHandler) handleOfficialStarHistoryEmbed(w http.ResponseWriter, r *http.Request, owner, repo string, metadata serving.RepositoryMetadata, theme card.Theme, locale card.Locale) {
	cached, err := h.loadOfficialHistory(r.Context(), owner, repo)
	if err != nil {
		h.writeOfficialHistoryError(w, err)
		return
	}
	events, err := series.OfficialEvents(cached.Weeks)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CORRUPT_HISTORY", "Cached GitHub star history is invalid.", nil)
		return
	}
	points, err := series.NormalizeOfficial(events, metadata.CurrentStars)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "NORMALIZE_FAILED", "Unable to normalize star history.", nil)
		return
	}
	if len(points) < 2 {
		writeError(w, http.StatusNotFound, "HISTORY_NOT_FOUND", "Star history is not available for this repository.", nil)
		return
	}
	fullName := metadata.FullName
	if strings.TrimSpace(fullName) == "" {
		fullName = owner + "/" + repo
	}
	input := card.RenderInput{
		FullName: fullName, Description: metadata.Description, Language: metadata.Language,
		Topics: metadata.Topics, CurrentStars: metadata.CurrentStars, CreatedAt: metadata.CreatedAt,
		AvatarDataURI: metadata.AvatarDataURI, CoverageStart: parsePointDate(points[0].Date),
		GeneratedAt: cached.FetchedAt, Points: points, Theme: theme, Locale: locale,
	}
	content, err := card.Render(input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "RENDER_FAILED", "Unable to render star history.", nil)
		return
	}
	etag := officialEmbedETag(owner, repo, metadata.RepoID, metadata.CurrentStars, cached, theme, locale)
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", embedCacheControl)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// parsePointDate 只接收 NormalizeOfficial 生成的 ISO 日期。保留 UTC epoch 回退，
// 避免损坏缓存让 renderer panic；周数据校验仍会在更早阶段拒绝非法 payload。
func parsePointDate(value string) time.Time {
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Unix(0, 0).UTC()
	}
	return parsed.UTC()
}

func (h *HistoryHandler) loadOfficialHistory(ctx context.Context, owner, repo string) (model.GitHubStarHistoryCache, error) {
	key := strings.ToLower(strings.TrimSpace(owner) + "/" + strings.TrimSpace(repo))
	now := h.now().UTC()
	if value, ok := h.memory.Get(key, now); ok {
		return value.(model.GitHubStarHistoryCache), nil
	}
	returnValue, err := h.flights.Do(ctx, key, func() (any, error) {
		if value, ok := h.memory.Get(key, h.now().UTC()); ok {
			return value, nil
		}
		var cached model.GitHubStarHistoryCache
		var found bool
		if h.historyCache != nil {
			var err error
			cached, found, err = h.historyCache.GitHubStarHistoryCache(ctx, owner, repo)
			if err != nil {
				return nil, fmt.Errorf("read github star history cache: %w", err)
			}
		}
		now := h.now().UTC()
		if found && now.Sub(cached.FetchedAt) < officialHistoryCacheTTL {
			h.memory.Set(key, cached, now, h.officialMemoryCacheTTL)
			return cached, nil
		}
		refreshed, err := h.refreshOfficialHistory(ctx, owner, repo, cached, found, now)
		if err != nil {
			// 官方 API 临时限流或网络抖动时，已验证的旧曲线比让 README 直接
			// 失败更有价值。不要推进 DB 的 fetched_at，短暂内存兜底后仍会重试。
			if found && len(cached.Weeks) > 0 {
				h.memory.Set(key, cached, now, time.Minute)
				return cached, nil
			}
			return nil, err
		}
		h.memory.Set(key, refreshed, now, h.officialMemoryCacheTTL)
		return refreshed, nil
	})
	if err != nil {
		return model.GitHubStarHistoryCache{}, err
	}
	return returnValue.(model.GitHubStarHistoryCache), nil
}

func (h *HistoryHandler) refreshOfficialHistory(ctx context.Context, owner, repo string, cached model.GitHubStarHistoryCache, found bool, now time.Time) (model.GitHubStarHistoryCache, error) {
	if found && !cached.FullHistoryValidatedAt.IsZero() && now.Sub(cached.FullHistoryValidatedAt) < fullHistoryRefreshAfter {
		page, err := h.starHistory.StarHistory(ctx, owner, repo, 1, officialHistoryPerPage, cached.ResponseETag)
		if err != nil {
			return model.GitHubStarHistoryCache{}, err
		}
		if page.NotModified {
			cached.FetchedAt = now
			if page.ResponseETag != "" {
				cached.ResponseETag = page.ResponseETag
			}
			if h.historyCache != nil {
				if err := h.historyCache.TouchGitHubStarHistoryCache(ctx, owner, repo, now); err != nil {
					return model.GitHubStarHistoryCache{}, fmt.Errorf("touch github star history cache: %w", err)
				}
			}
			return cached, nil
		}
		weeks := append(append([]model.GitHubStarHistoryWeek(nil), cached.Weeks...), page.Weeks...)
		weeks, err = canonicalizeOfficialWeeks(weeks)
		if err != nil {
			return model.GitHubStarHistoryCache{}, err
		}
		cached.Weeks, cached.FetchedAt = weeks, now
		if page.ResponseETag != "" {
			cached.ResponseETag = page.ResponseETag
		}
		if h.historyCache != nil {
			if err := h.historyCache.SaveGitHubStarHistoryCache(ctx, owner, repo, cached); err != nil {
				return model.GitHubStarHistoryCache{}, fmt.Errorf("save github star history cache: %w", err)
			}
		}
		return cached, nil
	}

	weeks, etag, err := h.fetchCompleteOfficialHistory(ctx, owner, repo)
	if err != nil {
		return model.GitHubStarHistoryCache{}, err
	}
	if len(weeks) == 0 {
		return model.GitHubStarHistoryCache{}, provider.ErrNotFound
	}
	refreshed := model.GitHubStarHistoryCache{
		Weeks: weeks, ResponseETag: etag, FetchedAt: now, FullHistoryValidatedAt: now,
	}
	if h.historyCache != nil {
		if err := h.historyCache.SaveGitHubStarHistoryCache(ctx, owner, repo, refreshed); err != nil {
			return model.GitHubStarHistoryCache{}, fmt.Errorf("save github star history cache: %w", err)
		}
	}
	return refreshed, nil
}

func (h *HistoryHandler) fetchCompleteOfficialHistory(ctx context.Context, owner, repo string) ([]model.GitHubStarHistoryWeek, string, error) {
	weeks := make([]model.GitHubStarHistoryWeek, 0)
	etag := ""
	for page := 1; page <= officialHistoryMaxPages; page++ {
		response, err := h.starHistory.StarHistory(ctx, owner, repo, page, officialHistoryPerPage, "")
		if err != nil {
			return nil, "", err
		}
		if page == 1 {
			etag = response.ResponseETag
		}
		if len(response.Weeks) == 0 {
			break
		}
		weeks = append(weeks, response.Weeks...)
	}
	canonical, err := canonicalizeOfficialWeeks(weeks)
	return canonical, etag, err
}

func canonicalizeOfficialWeeks(weeks []model.GitHubStarHistoryWeek) ([]model.GitHubStarHistoryWeek, error) {
	if _, err := series.OfficialEvents(weeks); err != nil {
		return nil, err
	}
	byWeek := make(map[int64]model.GitHubStarHistoryWeek, len(weeks))
	for _, week := range weeks {
		byWeek[week.Week] = week
	}
	result := make([]model.GitHubStarHistoryWeek, 0, len(byWeek))
	for _, week := range byWeek {
		result = append(result, week)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Week < result[j].Week })
	return result, nil
}

func (h *HistoryHandler) writeOfficialHistoryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, provider.ErrNotFound):
		writeError(w, http.StatusNotFound, "HISTORY_NOT_FOUND", "Star history is not available for this repository.", nil)
	case errors.Is(err, provider.ErrRateLimited):
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "GITHUB_RATE_LIMITED", "GitHub star history is temporarily unavailable.", nil)
	default:
		writeError(w, http.StatusServiceUnavailable, "GITHUB_ERROR", "Unable to refresh GitHub star history.", nil)
	}
}

func officialHistoryETag(owner, repo string, repoID int64, currentStars int, historyRange model.HistoryRange, value model.GitHubStarHistoryCache) string {
	return hashETag("official", owner, repo, fmt.Sprint(repoID), fmt.Sprint(currentStars), string(historyRange), cacheFingerprint(value))
}

func officialEmbedETag(owner, repo string, repoID int64, currentStars int, value model.GitHubStarHistoryCache, theme card.Theme, locale card.Locale) string {
	return hashETag("official-embed", owner, repo, fmt.Sprint(repoID), fmt.Sprint(currentStars), string(theme), string(locale), card.RendererVersion, cacheFingerprint(value))
}

func cacheFingerprint(value model.GitHubStarHistoryCache) string {
	payload, _ := json.Marshal(value.Weeks)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:16]) + "|" + value.ResponseETag
}

func hashETag(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}
