package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/provider"
	"github.com/starcat-app/starcat-history-api/internal/serving"
	"github.com/starcat-app/starcat-history-api/internal/telemetry"
)

// scriptedMetadataProvider 让测试可以精确控制"回源会返回什么"以及"是否阻塞"。
type scriptedMetadataProvider struct {
	calls   atomic.Int64
	result  func() (serving.RepositoryMetadata, error)
	release chan struct{}
}

func (p *scriptedMetadataProvider) Fetch(context.Context, string, string) (serving.RepositoryMetadata, error) {
	p.calls.Add(1)
	if p.release != nil {
		<-p.release
	}
	return p.result()
}

func newStore(t *testing.T) *serving.Store {
	t.Helper()
	store, err := serving.Open(t.TempDir() + "/history.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// 曲线接口不传 repo_id 时 repoID 为 0，缓存必须按 full name 命中。
// 这是线上 8.5s/次 那条路径的回归测试：缓存写进去了，但读取端拿的是 repo_id=0。
func TestMetadataCacheIsReadWithoutRepoID(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	if err := store.SaveMetadata(context.Background(), serving.RepositoryMetadata{
		RepoID: 42, FullName: "owner/repo", Visibility: "public", CurrentStars: 100, CheckedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	scripted := &scriptedMetadataProvider{result: func() (serving.RepositoryMetadata, error) {
		return serving.RepositoryMetadata{}, errors.New("must not reach GitHub")
	}}
	handler := NewHistoryHandler(store, scripted, time.Hour, 400)
	handler.now = func() time.Time { return now }

	metadata, err := handler.resolveMetadataCached(context.Background(), 0, "owner", "repo")
	if err != nil {
		t.Fatalf("cached metadata must be readable without repo_id: %v", err)
	}
	if metadata.RepoID != 42 || metadata.CurrentStars != 100 {
		t.Fatalf("unexpected metadata: %+v", metadata)
	}
	if scripted.calls.Load() != 0 {
		t.Fatalf("cache hit must not reach GitHub, got %d calls", scripted.calls.Load())
	}
}

// 调用方指名的 repo_id 与按名字命中的行不一致时，不能把别人的数据当命中返回。
func TestMetadataCacheRejectsRepoIDMismatch(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	if err := store.SaveMetadata(context.Background(), serving.RepositoryMetadata{
		RepoID: 42, FullName: "owner/repo", Visibility: "public", CurrentStars: 100, CheckedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 回源拿到的是 owner/repo 的真实 id（42），而调用方谎报 99：
	// 这正是"缓存里 id 与路径对不上"时必须拒绝的场景。
	scripted := &scriptedMetadataProvider{result: func() (serving.RepositoryMetadata, error) {
		return serving.RepositoryMetadata{RepoID: 42, FullName: "owner/repo", Visibility: "public", CurrentStars: 100, CheckedAt: now}, nil
	}}
	handler := NewHistoryHandler(store, scripted, time.Hour, 400)
	handler.now = func() time.Time { return now }

	if _, err := handler.resolveMetadataCached(context.Background(), 99, "owner", "repo"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("mismatched repo_id must be rejected, got %v", err)
	}
	if scripted.calls.Load() != 1 {
		t.Fatalf("mismatch must be re-verified against GitHub, got %d calls", scripted.calls.Load())
	}
}

// 同一仓库的并发冷启动必须合并成一次 GitHub 调用：README 首次被看到时就是这种突发。
func TestConcurrentMetadataLookupsShareOneGitHubCall(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	release := make(chan struct{})
	scripted := &scriptedMetadataProvider{
		release: release,
		result: func() (serving.RepositoryMetadata, error) {
			return serving.RepositoryMetadata{
				RepoID: 42, FullName: "owner/repo", Visibility: "public", CurrentStars: 100, CheckedAt: now,
			}, nil
		},
	}
	handler := NewHistoryHandler(store, scripted, time.Hour, 400)
	handler.now = func() time.Time { return now }

	const callers = 8
	var waitGroup sync.WaitGroup
	waitGroup.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer waitGroup.Done()
			if _, err := handler.resolveMetadataCached(context.Background(), 0, "owner", "repo"); err != nil {
				t.Errorf("concurrent resolve failed: %v", err)
			}
		}()
	}
	// 等所有调用者都进入等待，再放行回源，确保它们真的并发。
	deadline := time.Now().Add(2 * time.Second)
	for scripted.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	waitGroup.Wait()

	if got := scripted.calls.Load(); got != 1 {
		t.Fatalf("%d concurrent lookups must collapse into 1 GitHub call, got %d", callers, got)
	}
}

// 不存在的仓库必须留负缓存：公开入口可以被任意 owner/repo 刷。
func TestNotFoundIsNegativelyCached(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	scripted := &scriptedMetadataProvider{result: func() (serving.RepositoryMetadata, error) {
		return serving.RepositoryMetadata{}, provider.ErrNotFound
	}}
	registry := telemetry.NewRegistry()
	handler := NewHistoryHandler(store, scripted, time.Hour, 400, WithTelemetry(registry))
	handler.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if _, err := handler.resolveMetadataCached(context.Background(), 0, "owner", "ghost"); !errors.Is(err, provider.ErrNotFound) {
			t.Fatalf("call %d must report not found, got %v", i, err)
		}
	}
	if got := scripted.calls.Load(); got != 1 {
		t.Fatalf("repeat lookups must be served by the negative cache, got %d GitHub calls", got)
	}
	if snapshot := registry.Snapshot(); snapshot.MetadataNegativeHits != 2 {
		t.Fatalf("expected 2 negative cache hits, got %+v", snapshot)
	}
}

// 私有仓库同样进负缓存：旧实现里"非 public 不落库"，于是每个请求都会重新回源。
func TestPrivateRepositoryIsNegativelyCached(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	scripted := &scriptedMetadataProvider{result: func() (serving.RepositoryMetadata, error) {
		return serving.RepositoryMetadata{
			RepoID: 42, FullName: "owner/secret", Visibility: "private", CurrentStars: 5, CheckedAt: now,
		}, nil
	}}
	handler := NewHistoryHandler(store, scripted, time.Hour, 400)
	handler.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		metadata, err := handler.resolveMetadataCached(context.Background(), 0, "owner", "secret")
		if err != nil {
			t.Fatalf("call %d failed: %v", i, err)
		}
		if metadata.Visibility != "private" {
			t.Fatalf("call %d must stay non-public, got %q", i, metadata.Visibility)
		}
	}
	if got := scripted.calls.Load(); got != 1 {
		t.Fatalf("private repository must be negatively cached, got %d GitHub calls", got)
	}
}

// 负缓存必须有期限：转公开、元数据修正都要能在 TTL 之后被重新确认。
func TestNegativeCacheExpires(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	scripted := &scriptedMetadataProvider{result: func() (serving.RepositoryMetadata, error) {
		return serving.RepositoryMetadata{}, provider.ErrNotFound
	}}
	handler := NewHistoryHandler(store, scripted, time.Hour, 400, WithNegativeCacheTTL(30*time.Minute))
	handler.now = func() time.Time { return now }

	if _, err := handler.resolveMetadataCached(context.Background(), 0, "owner", "ghost"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("first lookup must report not found, got %v", err)
	}
	// TTL 内不再回源。
	now = now.Add(29 * time.Minute)
	if _, err := handler.resolveMetadataCached(context.Background(), 0, "owner", "ghost"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("lookup inside TTL must report not found, got %v", err)
	}
	if got := scripted.calls.Load(); got != 1 {
		t.Fatalf("negative cache must suppress lookups inside the TTL, got %d calls", got)
	}
	// 过期后必须重新确认。
	now = now.Add(2 * time.Minute)
	if _, err := handler.resolveMetadataCached(context.Background(), 0, "owner", "ghost"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("lookup after TTL must report not found, got %v", err)
	}
	if got := scripted.calls.Load(); got != 2 {
		t.Fatalf("expired negative cache must re-check GitHub, got %d calls", got)
	}
}

// 仓库从"被判不可用"变成可取之后，负缓存必须被清掉，否则 TTL 内会一直被拒。
func TestPublicMetadataClearsNegativeCache(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	var public atomic.Bool
	scripted := &scriptedMetadataProvider{result: func() (serving.RepositoryMetadata, error) {
		if !public.Load() {
			return serving.RepositoryMetadata{}, provider.ErrNotFound
		}
		return serving.RepositoryMetadata{
			RepoID: 42, FullName: "owner/repo", Visibility: "public", CurrentStars: 100, CheckedAt: now,
		}, nil
	}}
	handler := NewHistoryHandler(store, scripted, time.Hour, 400, WithNegativeCacheTTL(30*time.Minute))
	handler.now = func() time.Time { return now }

	if _, err := handler.resolveMetadataCached(context.Background(), 0, "owner", "repo"); err == nil {
		t.Fatal("first lookup should fail")
	}
	public.Store(true)
	now = now.Add(time.Hour)
	if _, err := handler.resolveMetadataCached(context.Background(), 0, "owner", "repo"); err != nil {
		t.Fatalf("repository must be picked up once it is public: %v", err)
	}
	if _, _, found, err := store.NegativeMetadata(context.Background(), "owner/repo"); err != nil || found {
		t.Fatalf("negative cache must be cleared after a successful public fetch: found=%v err=%v", found, err)
	}
}

// 公开入口拿未公开仓库的 repo_id 猜路径时，仍要走既有的 422 分支而不是 409/500。
func TestPrivateNegativeCacheKeepsPublicGateOnCurveEndpoint(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	scripted := &scriptedMetadataProvider{result: func() (serving.RepositoryMetadata, error) {
		return serving.RepositoryMetadata{
			RepoID: 42, FullName: "owner/secret", Visibility: "private", CurrentStars: 5, CheckedAt: now,
		}, nil
	}}
	// 必须装配官方历史 provider，否则 HandleStarHistory 会走 legacy 序列路径，
	// 在可见性校验之前就因为"没有 WatchEvent 序列"返回 404。
	handler := NewHistoryHandler(store, scripted, time.Hour, 400, WithStarHistoryProvider(&fakeStarHistoryProvider{}))
	handler.now = func() time.Time { return now }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/star-history", handler.HandleStarHistory)

	for i := 0; i < 2; i++ {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/repos/owner/secret/star-history?repo_id=42", nil))
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("call %d: private repository must be rejected with 422, got %d: %s", i, response.Code, response.Body.String())
		}
	}
	if got := scripted.calls.Load(); got != 1 {
		t.Fatalf("second call must be served by the negative cache, got %d GitHub calls", got)
	}
}
