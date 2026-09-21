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
	"sync"
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
	// officialHistoryFetchConcurrency 是单仓库冷启动时的并行分页数。
	// 取 4 是按"成熟仓库 20 页左右"估的：把 13 秒压到 3 秒以内，同时不至于让一次
	// 冷启动就把 GitHub 并发配额吃满（全局闸门在下一个阶段）。
	officialHistoryFetchConcurrency = 4
)

// staleCacheHeader 标注"这次返回的是回源失败后的旧数据"。
// 用独立响应头而不是改 Cache-Control：客户端与 CDN 的缓存策略不变，
// 运维侧却能一次 curl 就看出数据是不是旧的。
const staleCacheHeader = "X-Starcat-Cache"

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

	loaded, err := h.loadOfficialHistory(r.Context(), owner, repo)
	if err != nil {
		h.writeOfficialHistoryError(w, err)
		return
	}
	cached := loaded.value
	if loaded.stale {
		w.Header().Set(staleCacheHeader, "stale")
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
	if ifNoneMatch(r.Header.Get("If-None-Match"), etag) {
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
	loaded, err := h.loadOfficialHistory(r.Context(), owner, repo)
	if err != nil {
		h.writeOfficialHistoryError(w, err)
		return
	}
	cached := loaded.value
	if loaded.stale {
		w.Header().Set(staleCacheHeader, "stale")
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
	if ifNoneMatch(r.Header.Get("If-None-Match"), etag) {
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

// officialHistoryLoad 是一次历史读取的结果。
//
// stale 表示"这次吐的是旧曲线，因为回源失败了"。它必须是调用方能感知的信息：
// 对外要标注（X-Starcat-Cache: stale），否则线上看到的是"响应正常但数据偏旧"，
// 排查时无从下手。
type officialHistoryLoad struct {
	value model.GitHubStarHistoryCache
	stale bool
}

func (h *HistoryHandler) loadOfficialHistory(ctx context.Context, owner, repo string) (officialHistoryLoad, error) {
	key := strings.ToLower(strings.TrimSpace(owner) + "/" + strings.TrimSpace(repo))
	now := h.now().UTC()
	if value, ok := h.memory.Get(key, now); ok {
		h.telemetry.HistoryCacheHit()
		return value.(officialHistoryLoad), nil
	}
	returnValue, err := h.flights.Do(ctx, key, func() (any, error) {
		if value, ok := h.memory.Get(key, h.now().UTC()); ok {
			// 并发冷启动里后到的请求：虽然走了 singleflight，但它同样没有回源
			// GitHub，计入命中才能让「命中率」反映真实的回源压力。
			h.telemetry.HistoryCacheHit()
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
			h.telemetry.HistoryCacheHit()
			h.memory.Set(key, officialHistoryLoad{value: cached}, now, h.officialMemoryCacheTTL)
			return officialHistoryLoad{value: cached}, nil
		}
		// 退避窗口内不再打 GitHub：故障期间的重试本身就是放大器。
		if ok, retryAfter := h.backoff.allow(key, now); !ok {
			if found && len(cached.Weeks) > 0 {
				h.telemetry.HistoryStaleServed()
				stale := officialHistoryLoad{value: cached, stale: true}
				h.memory.Set(key, stale, now, retryAfter)
				return stale, nil
			}
			// 没有旧数据可吐：按"暂时不可用"处理，让调用方回 429 而不是错误的 404。
			return nil, provider.ErrRateLimited
		}
		h.telemetry.HistoryCacheMiss()
		refreshed, err := h.refreshOfficialHistory(ctx, owner, repo, cached, found, now)
		if err != nil {
			window := h.backoff.failure(key, now)
			// 官方 API 临时限流或网络抖动时，已验证的旧曲线比让 README 直接
			// 失败更有价值。不推进 DB 的 fetched_at，退避窗口内继续吐旧图。
			if found && len(cached.Weeks) > 0 {
				h.telemetry.HistoryStaleServed()
				stale := officialHistoryLoad{value: cached, stale: true}
				h.memory.Set(key, stale, now, window)
				return stale, nil
			}
			return nil, err
		}
		h.backoff.success(key)
		loaded := officialHistoryLoad{value: refreshed}
		h.memory.Set(key, loaded, now, h.officialMemoryCacheTTL)
		return loaded, nil
	})
	if err != nil {
		return officialHistoryLoad{}, err
	}
	return returnValue.(officialHistoryLoad), nil
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

// fetchCompleteOfficialHistory 拉取完整官方周历史。
//
// 两条路径：
//   - Link 头给出了 rel="last"（生产环境正常情况）：第 2 页起有界并行拉取。
//     这是冷启动从 13 秒降到 3 秒以内的关键 —— 成熟仓库有 20 页以上，串行拉
//     每页 0.5 秒，光排队就十几秒，而 README 卡片的第一次展示等不起。
//   - Link 缺失：退回"逐页拉到空页"的顺序语义，保持对 mock / 旧对端的兼容。
func (h *HistoryHandler) fetchCompleteOfficialHistory(ctx context.Context, owner, repo string) ([]model.GitHubStarHistoryWeek, string, error) {
	first, err := h.starHistory.StarHistory(ctx, owner, repo, 1, officialHistoryPerPage, "")
	if err != nil {
		return nil, "", err
	}
	etag := first.ResponseETag
	if len(first.Weeks) == 0 {
		return nil, etag, nil
	}
	weeks := first.Weeks

	lastPage := first.LastPage
	if lastPage < 2 {
		if lastPage == 0 {
			// 对端没给页数：顺序拉到空页为止。
			for page := 2; page <= officialHistoryMaxPages; page++ {
				response, err := h.starHistory.StarHistory(ctx, owner, repo, page, officialHistoryPerPage, "")
				if err != nil {
					return nil, "", err
				}
				if len(response.Weeks) == 0 {
					break
				}
				weeks = append(weeks, response.Weeks...)
			}
		}
		canonical, err := canonicalizeOfficialWeeks(weeks)
		return canonical, etag, err
	}
	if lastPage > officialHistoryMaxPages {
		lastPage = officialHistoryMaxPages
	}
	rest, err := h.fetchOfficialHistoryPages(ctx, owner, repo, 2, lastPage)
	if err != nil {
		return nil, "", err
	}
	weeks = append(weeks, rest...)
	canonical, err := canonicalizeOfficialWeeks(weeks)
	return canonical, etag, err
}

// fetchOfficialHistoryPages 以 officialHistoryFetchConcurrency 为上限并行拉取 [from, to]。
//
// 失败即整体失败，不做部分提交：半份历史比没有历史更难排查（曲线会突然少一段）。
// 取消只作用于尚未发出的请求，已经回来的页会被丢弃。
func (h *HistoryHandler) fetchOfficialHistoryPages(ctx context.Context, owner, repo string, from, to int) ([]model.GitHubStarHistoryWeek, error) {
	if to < from {
		return nil, nil
	}
	parallelCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	slots := make(chan struct{}, officialHistoryFetchConcurrency)
	pages := make([][]model.GitHubStarHistoryWeek, to-from+1)
	var (
		waitGroup sync.WaitGroup
		firstErr  error
		errOnce   sync.Once
	)
	for page := from; page <= to; page++ {
		waitGroup.Add(1)
		go func(page int) {
			defer waitGroup.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-parallelCtx.Done():
				return
			}
			response, err := h.starHistory.StarHistory(parallelCtx, owner, repo, page, officialHistoryPerPage, "")
			if err != nil {
				errOnce.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}
			pages[page-from] = response.Weeks
		}(page)
	}
	waitGroup.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	weeks := make([]model.GitHubStarHistoryWeek, 0)
	for _, chunk := range pages {
		weeks = append(weeks, chunk...)
	}
	return weeks, nil
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
	case isUpstreamBusy(err):
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
