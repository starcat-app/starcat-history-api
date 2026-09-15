// Package provider 获取查询期所需的 GitHub 当前公开元数据。
package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/starcat-app/starcat-api-kit/tokenpool"
	"github.com/starcat-app/starcat-history-api/internal/model"
	"github.com/starcat-app/starcat-history-api/internal/serving"
	"github.com/starcat-app/starcat-history-api/internal/telemetry"
)

var (
	ErrNotFound    = errors.New("repository not found")
	ErrRateLimited = errors.New("github rate limited")
)

const maximumAvatarBytes = 512 << 10

// MetadataProvider 隔离外部 GitHub API，便于 handler 单测和未来替换数据源。
type MetadataProvider interface {
	Fetch(context.Context, string, string) (serving.RepositoryMetadata, error)
}

// StarHistoryProvider 读取 GitHub 官方仓库 Star 历史。分页和 ETag 由调用方控制，
// 这样 handler 才能根据 DB 缓存年龄选择增量刷新或全量校验。
type StarHistoryProvider interface {
	StarHistory(context.Context, string, string, int, int, string) (model.GitHubStarHistoryWeekResponse, error)
}

// GitHubProvider 调用官方 REST API 获取当前 star 数和可见性。
type GitHubProvider struct {
	endpoint string
	// pool 复用 api-kit 的 GitHub PAT 池：quota-aware 选 token、401/5xx 死
	// token 检测、限流临时禁用。池为空（未配置 token）时所有请求匿名发送。
	pool   *tokenpool.Pool
	client *http.Client
	now    func() time.Time
	// telemetry 可为 nil（测试与未装配埋点的调用方），所有计数都走 nil-safe 方法。
	telemetry *telemetry.Registry
}

// WithTelemetry 注入计数指标；返回自身便于在装配处链式调用。
func (p *GitHubProvider) WithTelemetry(registry *telemetry.Registry) *GitHubProvider {
	p.telemetry = registry
	return p
}

// NewGitHubProvider 创建带超时的 GitHub Provider。token 支持逗号分隔的多值
// （对应 GITHUB_TOKENS 环境变量），空条目会被池自动忽略。
func NewGitHubProvider(endpoint, token string, client *http.Client) *GitHubProvider {
	if strings.TrimSpace(endpoint) == "" {
		endpoint = "https://api.github.com"
	}
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &GitHubProvider{
		endpoint: strings.TrimRight(endpoint, "/"),
		pool:     tokenpool.New(strings.Split(token, ",")),
		client:   client,
		now:      time.Now,
	}
}

// applyAuth 从池中选 token 写入 Authorization；返回 nil 表示匿名请求
// （未配置 token）或池耗尽（已带 ErrRateLimited）。
func (p *GitHubProvider) applyAuth(request *http.Request) (*tokenpool.TokenState, error) {
	if p.pool.Count() == 0 {
		return nil, nil
	}
	token := p.pool.PickBest()
	if token == nil {
		return nil, ErrRateLimited
	}
	request.Header.Set("Authorization", "Bearer "+token.Value)
	return token, nil
}

// handleRateLimited 在 403/429 时把该 token 按 Retry-After / reset 临时禁用，
// 与 api-kit 各服务的处理保持一致；匿名请求（token == nil）无需禁用。
func (p *GitHubProvider) handleRateLimited(token *tokenpool.TokenState, resp *http.Response) {
	if token == nil {
		return
	}
	pauseUntil := token.ResetAt
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
		if secs, err := strconv.Atoi(retryAfter); err == nil && secs > 0 {
			if ra := time.Now().Add(time.Duration(secs) * time.Second); ra.After(pauseUntil) {
				pauseUntil = ra
			}
		}
	}
	if pauseUntil.Before(time.Now().Add(60 * time.Second)) {
		pauseUntil = time.Now().Add(60 * time.Second)
	}
	log.Printf("[github] rate limited (%d), disabling token until %s", resp.StatusCode, pauseUntil.Format(time.RFC3339))
	p.telemetry.RateLimited()
	p.pool.DisableUntil(token, pauseUntil, fmt.Sprintf("rate limited status %d", resp.StatusCode))
}

// Fetch 获取公开仓库元数据。repo_id 最终仍由 handler 与 Serving 数据核对。
func (p *GitHubProvider) Fetch(ctx context.Context, owner, repo string) (serving.RepositoryMetadata, error) {
	target := p.endpoint + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return serving.RepositoryMetadata{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "starcat-history-api")
	poolToken, authErr := p.applyAuth(request)
	if authErr != nil {
		return serving.RepositoryMetadata{}, authErr
	}
	// 计数放在真正发请求之前：这里统计的是「对外产生了多少次 GitHub 调用」，
	// 失败也要计，否则限流期间的调用量会被低估。
	p.telemetry.MetadataRequested()
	response, err := p.client.Do(request)
	if err != nil {
		return serving.RepositoryMetadata{}, err
	}
	defer response.Body.Close()
	if poolToken != nil {
		p.pool.UpdateFromResponse(poolToken, response)
	}
	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests {
		p.handleRateLimited(poolToken, response)
		return serving.RepositoryMetadata{}, ErrRateLimited
	}
	if response.StatusCode == http.StatusNotFound {
		return serving.RepositoryMetadata{}, ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return serving.RepositoryMetadata{}, fmt.Errorf("github returned status %d", response.StatusCode)
	}
	var payload struct {
		ID              int64    `json:"id"`
		FullName        string   `json:"full_name"`
		Private         bool     `json:"private"`
		Visibility      string   `json:"visibility"`
		StargazersCount int      `json:"stargazers_count"`
		Description     string   `json:"description"`
		Language        string   `json:"language"`
		Topics          []string `json:"topics"`
		CreatedAt       string   `json:"created_at"`
		AvatarURL       string   `json:"avatar_url"`
		Owner           struct {
			AvatarURL string `json:"avatar_url"`
		} `json:"owner"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return serving.RepositoryMetadata{}, err
	}
	visibility := strings.ToLower(strings.TrimSpace(payload.Visibility))
	if payload.Private || visibility == "private" || visibility == "internal" {
		visibility = "private"
	} else {
		visibility = "public"
	}
	createdAt, err := time.Parse(time.RFC3339, payload.CreatedAt)
	if err != nil {
		return serving.RepositoryMetadata{}, fmt.Errorf("parse github created_at: %w", err)
	}
	avatarURL := payload.Owner.AvatarURL
	if avatarURL == "" {
		avatarURL = payload.AvatarURL
	}
	return serving.RepositoryMetadata{
		RepoID: payload.ID, FullName: payload.FullName, Visibility: visibility,
		CurrentStars: payload.StargazersCount, CheckedAt: p.now().UTC(),
		Description: payload.Description, Language: payload.Language, Topics: payload.Topics,
		CreatedAt: createdAt, AvatarURL: avatarURL,
		AvatarDataURI: p.fetchAvatarDataURI(ctx, avatarURL),
	}, nil
}

// StarHistory 请求 GitHub 官方 /stargazers/history 接口。
// GitHub 对该接口限制每页最多 30 条、最多 100 页；边界由 handler 的分页逻辑统一控制。
func (p *GitHubProvider) StarHistory(ctx context.Context, owner, repo string, page, perPage int, ifNoneMatch string) (model.GitHubStarHistoryWeekResponse, error) {
	if page < 1 || page > 100 || perPage < 1 || perPage > 30 {
		return model.GitHubStarHistoryWeekResponse{}, fmt.Errorf("invalid github star history pagination")
	}
	target := p.endpoint + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/stargazers/history"
	query := url.Values{}
	query.Set("page", strconv.Itoa(page))
	query.Set("per_page", strconv.Itoa(perPage))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"?"+query.Encode(), nil)
	if err != nil {
		return model.GitHubStarHistoryWeekResponse{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "starcat-history-api")
	if ifNoneMatch != "" {
		request.Header.Set("If-None-Match", ifNoneMatch)
	}
	poolToken, authErr := p.applyAuth(request)
	if authErr != nil {
		return model.GitHubStarHistoryWeekResponse{}, authErr
	}
	p.telemetry.HistoryRequested()
	response, err := p.client.Do(request)
	if err != nil {
		return model.GitHubStarHistoryWeekResponse{}, err
	}
	defer response.Body.Close()
	if poolToken != nil {
		p.pool.UpdateFromResponse(poolToken, response)
	}
	if response.StatusCode == http.StatusNotModified {
		return model.GitHubStarHistoryWeekResponse{NotModified: true, ResponseETag: response.Header.Get("ETag")}, nil
	}
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusUnprocessableEntity {
		return model.GitHubStarHistoryWeekResponse{}, ErrNotFound
	}
	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests {
		p.handleRateLimited(poolToken, response)
		return model.GitHubStarHistoryWeekResponse{}, ErrRateLimited
	}
	if response.StatusCode != http.StatusOK {
		return model.GitHubStarHistoryWeekResponse{}, fmt.Errorf("github returned status %d", response.StatusCode)
	}
	var weeks []model.GitHubStarHistoryWeek
	if err := json.NewDecoder(response.Body).Decode(&weeks); err != nil {
		return model.GitHubStarHistoryWeekResponse{}, err
	}
	return model.GitHubStarHistoryWeekResponse{Weeks: weeks, ResponseETag: response.Header.Get("ETag")}, nil
}

// fetchAvatarDataURI 将 GitHub owner avatar 内联到 SVG，保证 README 图片不依赖第二个远程资源。
// 头像是可选装饰资源：请求失败、类型不受支持或超过上限时直接回退到首字母占位，不能阻断历史卡片。
func (p *GitHubProvider) fetchAvatarDataURI(ctx context.Context, rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "avatars.githubusercontent.com") {
		return ""
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return ""
	}
	request.Header.Set("Accept", "image/png,image/jpeg,image/webp,image/gif")
	request.Header.Set("User-Agent", "starcat-history-api")
	p.telemetry.AvatarRequested()
	response, err := p.client.Do(request)
	if err != nil {
		return ""
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ""
	}
	mimeType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	switch mimeType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
	default:
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumAvatarBytes+1))
	if err != nil || len(data) == 0 || len(data) > maximumAvatarBytes {
		return ""
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}
