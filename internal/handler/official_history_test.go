package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/model"
	"github.com/starcat-app/starcat-history-api/internal/provider"
	"github.com/starcat-app/starcat-history-api/internal/serving"
)

type fakeStarHistoryProvider struct {
	pages    atomic.Int64
	seenETag atomic.Value
	page     func(int, string) model.GitHubStarHistoryWeekResponse
}

func (p *fakeStarHistoryProvider) StarHistory(_ context.Context, _ string, _ string, page int, _ int, etag string) (model.GitHubStarHistoryWeekResponse, error) {
	p.pages.Add(1)
	p.seenETag.Store(etag)
	return p.page(page, etag), nil
}

func TestOfficialHistoryUsesGitHubDataAndMemoryCache(t *testing.T) {
	store, err := serving.Open(t.TempDir() + "/history.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	if err := store.SaveMetadata(context.Background(), serving.RepositoryMetadata{
		RepoID: 42, FullName: "owner/repo", Visibility: "public", CurrentStars: 100, CheckedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	provider := &fakeStarHistoryProvider{page: func(page int, _ string) model.GitHubStarHistoryWeekResponse {
		if page == 1 {
			return model.GitHubStarHistoryWeekResponse{
				Weeks: []model.GitHubStarHistoryWeek{
					{Week: 1_725_696_000, Total: 3, Days: []int{1, 0, 2, 0, 0, 0, 0}},
					{Week: 1_725_091_200, Total: 1, Days: []int{0, 1, 0, 0, 0, 0, 0}},
				}, ResponseETag: `"history-v1"`,
			}
		}
		return model.GitHubStarHistoryWeekResponse{}
	}}
	handler := NewHistoryHandler(store, &fakeMetadataProvider{value: serving.RepositoryMetadata{
		RepoID: 42, FullName: "owner/repo", Visibility: "public", CurrentStars: 100, CheckedAt: now,
	}}, time.Hour, 400, WithStarHistoryProvider(provider))
	handler.now = func() time.Time { return now }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/star-history", handler.HandleStarHistory)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/owner/repo/star-history?range=all", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"source":"github_history"`) || !strings.Contains(response.Body.String(), `"precision":"reconstructed"`) {
		t.Fatalf("response did not use official source: %s", response.Body.String())
	}
	if provider.pages.Load() != 2 {
		t.Fatalf("expected full pagination on cold cache, got %d calls", provider.pages.Load())
	}
	second := httptest.NewRecorder()
	mux.ServeHTTP(second, request)
	if second.Code != http.StatusOK || provider.pages.Load() != 2 {
		t.Fatalf("memory cache did not avoid GitHub request: status=%d calls=%d", second.Code, provider.pages.Load())
	}
}

func TestOfficialHistoryUsesDBCacheAndETagRefresh(t *testing.T) {
	store, err := serving.Open(t.TempDir() + "/history.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	old := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := store.SaveMetadata(context.Background(), serving.RepositoryMetadata{
		RepoID: 42, FullName: "owner/repo", Visibility: "public", CurrentStars: 100, CheckedAt: time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGitHubStarHistoryCache(context.Background(), "owner", "repo", model.GitHubStarHistoryCache{
		Weeks:        []model.GitHubStarHistoryWeek{{Week: 1_725_091_200, Total: 1, Days: []int{1, 0, 0, 0, 0, 0, 0}}},
		ResponseETag: `"history-v1"`, FetchedAt: old, FullHistoryValidatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	provider := &fakeStarHistoryProvider{page: func(page int, etag string) model.GitHubStarHistoryWeekResponse {
		if page == 1 && etag == `"history-v1"` {
			return model.GitHubStarHistoryWeekResponse{NotModified: true}
		}
		return model.GitHubStarHistoryWeekResponse{}
	}}
	handler := NewHistoryHandler(store, &fakeMetadataProvider{value: serving.RepositoryMetadata{
		RepoID: 42, FullName: "owner/repo", Visibility: "public", CurrentStars: 100, CheckedAt: time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC),
	}}, time.Hour, 400, WithStarHistoryProvider(provider))
	handler.now = func() time.Time { return time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC) }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/star-history", handler.HandleStarHistory)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/owner/repo/star-history?range=all", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK || provider.pages.Load() != 1 {
		t.Fatalf("expected one conditional DB refresh, status=%d calls=%d body=%s", response.Code, provider.pages.Load(), response.Body.String())
	}
	cached, found, err := store.GitHubStarHistoryCache(context.Background(), "OWNER", "REPO")
	if err != nil || !found || !cached.FetchedAt.Equal(time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("304 did not touch DB cache: found=%v cache=%#v err=%v", found, cached, err)
	}
}

var _ provider.StarHistoryProvider = (*fakeStarHistoryProvider)(nil)

func TestOfficialMemoryCacheTTLIsConfigurable(t *testing.T) {
	defaultHandler := NewHistoryHandler(nil, nil, time.Hour, 400)
	if defaultHandler.officialMemoryCacheTTL != DefaultOfficialMemoryCacheTTL {
		t.Fatalf("unexpected default memory cache TTL: %s", defaultHandler.officialMemoryCacheTTL)
	}

	custom := 2 * time.Hour
	configuredHandler := NewHistoryHandler(nil, nil, time.Hour, 400, WithOfficialMemoryCacheTTL(custom))
	if configuredHandler.officialMemoryCacheTTL != custom {
		t.Fatalf("custom memory cache TTL was not applied: %s", configuredHandler.officialMemoryCacheTTL)
	}

	invalidHandler := NewHistoryHandler(nil, nil, time.Hour, 400, WithOfficialMemoryCacheTTL(0))
	if invalidHandler.officialMemoryCacheTTL != DefaultOfficialMemoryCacheTTL {
		t.Fatalf("invalid memory cache TTL must keep the default: %s", invalidHandler.officialMemoryCacheTTL)
	}
}

// 官方历史分支与 Serving 分支是同一个公开图片地址的两条实现路径，缓存策略必须
// 完全一致：否则同一张 README 卡片会因为部署形态不同而出现不同的新鲜度，用户
// 看到的星标数也会随之漂移。
func TestOfficialHistoryEmbedUsesSharedCachePolicy(t *testing.T) {
	store, err := serving.Open(t.TempDir() + "/history.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	metadata := serving.RepositoryMetadata{
		RepoID: 42, FullName: "owner/repo", Visibility: "public", CurrentStars: 100, CheckedAt: now,
	}
	if err := store.SaveMetadata(context.Background(), metadata); err != nil {
		t.Fatal(err)
	}
	starHistory := &fakeStarHistoryProvider{page: func(page int, _ string) model.GitHubStarHistoryWeekResponse {
		if page == 1 {
			return model.GitHubStarHistoryWeekResponse{
				Weeks: []model.GitHubStarHistoryWeek{
					{Week: 1_725_696_000, Total: 3, Days: []int{1, 0, 2, 0, 0, 0, 0}},
					{Week: 1_725_091_200, Total: 1, Days: []int{0, 1, 0, 0, 0, 0, 0}},
				}, ResponseETag: `"history-v1"`,
			}
		}
		return model.GitHubStarHistoryWeekResponse{}
	}}
	handler := NewHistoryHandler(store, &fakeMetadataProvider{value: metadata}, time.Hour, 400, WithStarHistoryProvider(starHistory))
	handler.now = func() time.Time { return now }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /embed/v1/repos/{owner}/{repo}/star-history.svg", handler.HandleStarHistoryEmbed)

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/embed/v1/repos/owner/repo/star-history.svg?theme=light&locale=en", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "image/svg+xml; charset=utf-8" {
		t.Fatalf("unexpected content type %q", response.Header().Get("Content-Type"))
	}
	if got := response.Header().Get("Cache-Control"); got != embedCacheControl {
		t.Fatalf("official embed must reuse the shared cache policy, got %q", got)
	}
	if values := response.Header().Values("Cache-Control"); len(values) != 1 {
		t.Fatalf("official embed response must carry exactly one Cache-Control value, got %v", values)
	}
	// 没有这一步，Serving 分支的兜底实现也能让断言通过，守卫就是假的。
	if starHistory.pages.Load() == 0 {
		t.Fatal("official history provider was never called, embed did not exercise the official branch")
	}
}
