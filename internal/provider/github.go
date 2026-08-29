// Package provider 获取查询期所需的 GitHub 当前公开元数据。
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/serving"
)

var (
	ErrNotFound    = errors.New("repository not found")
	ErrRateLimited = errors.New("github rate limited")
)

// MetadataProvider 隔离外部 GitHub API，便于 handler 单测和未来替换数据源。
type MetadataProvider interface {
	Fetch(context.Context, string, string) (serving.RepositoryMetadata, error)
}

// GitHubProvider 调用官方 REST API 获取当前 star 数和可见性。
type GitHubProvider struct {
	endpoint string
	token    string
	client   *http.Client
	now      func() time.Time
}

// NewGitHubProvider 创建带超时的 GitHub Provider。
func NewGitHubProvider(endpoint, token string, client *http.Client) *GitHubProvider {
	if strings.TrimSpace(endpoint) == "" {
		endpoint = "https://api.github.com"
	}
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &GitHubProvider{endpoint: strings.TrimRight(endpoint, "/"), token: strings.TrimSpace(token), client: client, now: time.Now}
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
	if p.token != "" {
		request.Header.Set("Authorization", "Bearer "+p.token)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return serving.RepositoryMetadata{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return serving.RepositoryMetadata{}, ErrNotFound
	}
	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests {
		return serving.RepositoryMetadata{}, ErrRateLimited
	}
	if response.StatusCode != http.StatusOK {
		return serving.RepositoryMetadata{}, fmt.Errorf("github returned status %d", response.StatusCode)
	}
	var payload struct {
		ID              int64  `json:"id"`
		FullName        string `json:"full_name"`
		Private         bool   `json:"private"`
		Visibility      string `json:"visibility"`
		StargazersCount int    `json:"stargazers_count"`
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
	return serving.RepositoryMetadata{
		RepoID: payload.ID, FullName: payload.FullName, Visibility: visibility,
		CurrentStars: payload.StargazersCount, CheckedAt: p.now().UTC(),
	}, nil
}
