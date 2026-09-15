// Package handler 提供 History API 的公开查询与 README SVG 嵌入入口。
package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/starcat-app/starcat-history-api/internal/card"
	"github.com/starcat-app/starcat-history-api/internal/provider"
	"github.com/starcat-app/starcat-history-api/internal/series"
	"github.com/starcat-app/starcat-history-api/internal/serving"
)

// embedCacheControl 是 SVG 嵌入响应唯一的缓存策略来源，两条分支（Serving 自研
// 曲线与 GitHub 官方历史）必须共用同一个值，否则同一张 README 图会因走哪条分支
// 而出现不同的新鲜度。
//
// 分层口径：浏览器 1 小时；Camo 等共享缓存 1 天；过期后允许先画旧图、后台再验证。
//
// stale-while-revalidate 故意只给 1 小时，不给更长的窗口：README 卡片上最显眼的
// 是「Total Stars」，浏览器冷加载时会先把过期副本画出来再后台换新，窗口开得越大
// 用户看到旧星标数的时间就越长（7 天窗口曾造成线上卡片显示两天前的星标数）。
// 1 小时仍能在源站被 GitHub 限流或重启时提供兜底。
//
// 约束：这里必须保持唯一写入者。Nginx 侧不得再用 add_header 追加第二个
// Cache-Control —— add_header 是追加而非覆盖，两个 max-age 并存属于未定义行为。
const embedCacheControl = "public, max-age=3600, s-maxage=86400, stale-while-revalidate=3600"

// HandleStarHistoryEmbed 处理公开 README 图片请求：
// GET /embed/v1/repos/{owner}/{repo}/star-history.svg
//
// 该路由故意不套 API_KEYS 鉴权，因为 GitHub README 的 <img> 请求不能携带自定义
// Authorization header。安全边界由 owner/repo 的 GitHub Public 校验、History
// Serving 中已发布的 repo-day 序列以及固定的 theme/locale 白名单共同承担。
func (h *HistoryHandler) HandleStarHistoryEmbed(w http.ResponseWriter, r *http.Request) {
	owner, repo, ok := parseEmbedRepository(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "INVALID_REPOSITORY", "Invalid repository path.", nil)
		return
	}
	theme, locale, ok := parseEmbedOptions(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "INVALID_EMBED_OPTIONS", "Unsupported theme or locale.", nil)
		return
	}

	metadata, err := h.resolveMetadataCached(r.Context(), 0, owner, repo)
	if err != nil {
		writeEmbedMetadataError(w, err)
		return
	}
	if metadata.Visibility != "public" || metadata.RepoID <= 0 {
		// 对外统一返回 404，不泄露私有仓库是否存在或是否曾进入 Serving。
		writeError(w, http.StatusNotFound, "REPOSITORY_NOT_FOUND", "Public repository history is not available.", nil)
		return
	}
	if h.starHistory != nil {
		h.handleOfficialStarHistoryEmbed(w, r, owner, repo, metadata, theme, locale)
		return
	}

	storedSeries, events, ok := h.loadDecodedSeries(w, r, metadata.RepoID)
	if !ok {
		return
	}
	points, err := series.Normalize(events, metadata.CurrentStars)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "NORMALIZE_FAILED", "Unable to normalize star history.", nil)
		return
	}
	if len(points) < 2 {
		writeError(w, http.StatusNotFound, "HISTORY_NOT_FOUND", "Star history is not available for this repository.", nil)
		return
	}
	active, ok := h.requireActive(w, r)
	if !ok {
		return
	}
	generatedAt := active.GeneratedAt
	if generatedAt.IsZero() {
		generatedAt = h.now().UTC()
	}
	fullName := metadata.FullName
	if strings.TrimSpace(fullName) == "" {
		fullName = owner + "/" + repo
	}
	input := card.RenderInput{
		FullName:      fullName,
		Description:   metadata.Description,
		Language:      metadata.Language,
		Topics:        metadata.Topics,
		CurrentStars:  metadata.CurrentStars,
		CreatedAt:     metadata.CreatedAt,
		AvatarDataURI: metadata.AvatarDataURI,
		CoverageStart: series.TimeFromDay(storedSeries.CoverageStartDay),
		GeneratedAt:   generatedAt,
		Points:        points,
		Theme:         theme,
		Locale:        locale,
	}
	content, err := card.Render(input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "RENDER_FAILED", "Unable to render star history.", nil)
		return
	}

	etag := embedETag(metadata.RepoID, metadata.CurrentStars, storedSeries.SeriesChecksum, active, theme, locale)
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

func parseEmbedRepository(r *http.Request) (owner, repo string, ok bool) {
	owner = strings.TrimSpace(r.PathValue("owner"))
	repo = strings.TrimSpace(r.PathValue("repo"))
	return owner, repo, repositoryPathPart.MatchString(owner) && repositoryPathPart.MatchString(repo)
}

func parseEmbedOptions(r *http.Request) (card.Theme, card.Locale, bool) {
	theme := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("theme")))
	if theme == "" {
		theme = string(card.ThemeLight)
	}
	locale := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("locale")))
	if locale == "" {
		locale = string(card.LocaleEnglish)
	}
	switch locale {
	case "en", "en-us":
		locale = string(card.LocaleEnglish)
	case "zh", "zh-cn", "zh-tw":
		locale = string(card.LocaleChinese)
	default:
		return "", "", false
	}
	if theme != string(card.ThemeLight) && theme != string(card.ThemeDark) {
		return "", "", false
	}
	return card.Theme(theme), card.Locale(locale), true
}

func writeEmbedMetadataError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, provider.ErrNotFound):
		writeError(w, http.StatusNotFound, "REPOSITORY_NOT_FOUND", "Public repository history is not available.", nil)
	case errors.Is(err, provider.ErrRateLimited):
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "GITHUB_RATE_LIMITED", "GitHub metadata is temporarily unavailable.", nil)
	default:
		writeError(w, http.StatusServiceUnavailable, "GITHUB_ERROR", "Unable to validate repository metadata.", nil)
	}
}

func embedETag(repoID int64, currentStars int, checksum string, active serving.ActiveState, theme card.Theme, locale card.Locale) string {
	value := fmt.Sprintf("embed|%d|%d|%s|%s|%s|%s|%s|%s", repoID, currentStars, checksum, active.ModelVersion, active.ActiveWatermark, theme, locale, card.RendererVersion)
	sum := sha256.Sum256([]byte(value))
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}
