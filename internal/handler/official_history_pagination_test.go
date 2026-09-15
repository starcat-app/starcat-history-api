package handler

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/model"
	"github.com/starcat-app/starcat-history-api/internal/provider"
)

// scriptedStarHistoryProvider 记录每次分页请求，并可控制延迟与失败。
type scriptedStarHistoryProvider struct {
	mu      sync.Mutex
	pages   []int
	delay   time.Duration
	handler func(page int) (model.GitHubStarHistoryWeekResponse, error)
}

func (p *scriptedStarHistoryProvider) StarHistory(
	ctx context.Context, _, _ string, page, _ int, _ string,
) (model.GitHubStarHistoryWeekResponse, error) {
	p.mu.Lock()
	p.pages = append(p.pages, page)
	p.mu.Unlock()
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return model.GitHubStarHistoryWeekResponse{}, ctx.Err()
		}
	}
	return p.handler(page)
}

func (p *scriptedStarHistoryProvider) requestedPages() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := append([]int(nil), p.pages...)
	sort.Ints(result)
	return result
}

func weeklyPage(page int, lastPage int) model.GitHubStarHistoryWeekResponse {
	return model.GitHubStarHistoryWeekResponse{
		Weeks: []model.GitHubStarHistoryWeek{
			{Week: int64(1_725_696_000 - (page-1)*604_800), Total: 1, Days: []int{0, 1, 0, 0, 0, 0, 0}},
		},
		ResponseETag: `"history-v1"`,
		LastPage:     lastPage,
	}
}

// Link 头给出总页数时，第 2 页起必须并行拉：串行拉 20 页是冷启动十几秒的来源。
func TestOfficialHistoryFetchesRemainingPagesInParallel(t *testing.T) {
	store := newStore(t)
	const lastPage = 8
	starHistory := &scriptedStarHistoryProvider{
		delay: 100 * time.Millisecond,
		handler: func(page int) (model.GitHubStarHistoryWeekResponse, error) {
			return weeklyPage(page, lastPage), nil
		},
	}
	handler := NewHistoryHandler(store, nil, time.Hour, 400, WithStarHistoryProvider(starHistory))

	start := time.Now()
	weeks, _, err := handler.fetchCompleteOfficialHistory(context.Background(), "owner", "repo")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(weeks) != lastPage {
		t.Fatalf("expected %d weeks, got %d", lastPage, len(weeks))
	}
	if pages := starHistory.requestedPages(); len(pages) != lastPage || pages[0] != 1 || pages[lastPage-1] != lastPage {
		t.Fatalf("unexpected page set: %v", pages)
	}
	// 8 页串行 = 800ms；并发 4 应在 200-300ms。留足余量避免 CI 抖动误报。
	if elapsed >= 600*time.Millisecond {
		t.Fatalf("pagination looks sequential: %d pages took %s", lastPage, elapsed)
	}
}

// 对端没给 Link 时退回顺序语义：保持对 mock 与旧对端的兼容。
func TestOfficialHistoryFallsBackToSequentialPaginationWithoutLink(t *testing.T) {
	store := newStore(t)
	starHistory := &scriptedStarHistoryProvider{handler: func(page int) (model.GitHubStarHistoryWeekResponse, error) {
		if page > 2 {
			// 第 3 页起为空：顺序语义靠"空页即结束"收敛。
			return model.GitHubStarHistoryWeekResponse{ResponseETag: `"history-v1"`}, nil
		}
		return weeklyPage(page, 0), nil
	}}
	handler := NewHistoryHandler(store, nil, time.Hour, 400, WithStarHistoryProvider(starHistory))

	weeks, _, err := handler.fetchCompleteOfficialHistory(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(weeks) != 2 {
		t.Fatalf("expected 2 weeks, got %d", len(weeks))
	}
	if pages := starHistory.requestedPages(); len(pages) != 3 {
		t.Fatalf("sequential fallback must stop at the first empty page, got pages %v", pages)
	}
}

// 分页失败必须整体失败：半份历史比没有历史更难排查（曲线会突然少一段）。
func TestOfficialHistoryPaginationFailureFailsWholeFetch(t *testing.T) {
	store := newStore(t)
	starHistory := &scriptedStarHistoryProvider{handler: func(page int) (model.GitHubStarHistoryWeekResponse, error) {
		if page == 4 {
			return model.GitHubStarHistoryWeekResponse{}, errors.New("github exploded")
		}
		return weeklyPage(page, 8), nil
	}}
	handler := NewHistoryHandler(store, nil, time.Hour, 400, WithStarHistoryProvider(starHistory))

	if _, _, err := handler.fetchCompleteOfficialHistory(context.Background(), "owner", "repo"); err == nil {
		t.Fatal("expected the whole fetch to fail")
	}
}

// 退避窗口内不再回源，并且继续对外吐旧曲线（附 stale 标注）。
func TestRefreshFailureServesStaleAndBacksOff(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	if err := store.SaveGitHubStarHistoryCache(context.Background(), "owner", "repo", model.GitHubStarHistoryCache{
		Weeks:     []model.GitHubStarHistoryWeek{{Week: 1_725_696_000, Total: 2, Days: []int{0, 0, 2, 0, 0, 0, 0}}},
		FetchedAt: now.Add(-48 * time.Hour), // 已过期 → 需要回源
	}); err != nil {
		t.Fatal(err)
	}
	starHistory := &scriptedStarHistoryProvider{handler: func(int) (model.GitHubStarHistoryWeekResponse, error) {
		return model.GitHubStarHistoryWeekResponse{}, errors.New("github down")
	}}
	handler := NewHistoryHandler(store, nil, time.Hour, 400, WithStarHistoryProvider(starHistory))
	handler.now = func() time.Time { return now }

	first, err := handler.loadOfficialHistory(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("stale data must still be served: %v", err)
	}
	if !first.stale {
		t.Fatal("first load after a failed refresh must be marked stale")
	}
	if len(first.value.Weeks) != 1 {
		t.Fatalf("expected cached weeks to be served, got %d", len(first.value.Weeks))
	}
	if got := starHistory.requestedPages(); len(got) != 1 {
		t.Fatalf("expected one refresh attempt, got %v", got)
	}

	// 退避窗口内：必须命中内存 stale 副本，不再打 GitHub。
	now = now.Add(time.Minute)
	second, err := handler.loadOfficialHistory(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if !second.stale {
		t.Fatal("served data must stay marked stale inside the backoff window")
	}
	if got := starHistory.requestedPages(); len(got) != 1 {
		t.Fatalf("backoff window must suppress retries, got %v", got)
	}

	// 窗口过后必须重试（GitHub 可能已恢复）。
	now = now.Add(backoffBase)
	if _, err := handler.loadOfficialHistory(context.Background(), "owner", "repo"); err != nil {
		t.Fatal(err)
	}
	if got := starHistory.requestedPages(); len(got) < 2 {
		t.Fatalf("expired backoff must retry, got %v", got)
	}
}

// 没有旧数据可吐且处于退避窗口时，返回"暂时不可用"而不是错误的 404 —— 后者会让
// 客户端把"上游故障"误判成"这个仓库没有历史"。
func TestRefreshFailureWithoutCacheReportsRateLimitedInsideBackoff(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	starHistory := &scriptedStarHistoryProvider{handler: func(int) (model.GitHubStarHistoryWeekResponse, error) {
		return model.GitHubStarHistoryWeekResponse{}, errors.New("github down")
	}}
	handler := NewHistoryHandler(store, nil, time.Hour, 400, WithStarHistoryProvider(starHistory))
	handler.now = func() time.Time { return now }

	if _, err := handler.loadOfficialHistory(context.Background(), "owner", "repo"); err == nil {
		t.Fatal("expected failure on the cold attempt")
	}
	_, err := handler.loadOfficialHistory(context.Background(), "owner", "repo")
	if !errors.Is(err, provider.ErrRateLimited) {
		t.Fatalf("inside the backoff window a cold repo must report rate limited, got %v", err)
	}
}

// 回源成功后必须清掉退避，否则恢复后仍会被窗口挡住。
func TestSuccessfulRefreshClearsBackoff(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	var healthy bool
	starHistory := &scriptedStarHistoryProvider{handler: func(page int) (model.GitHubStarHistoryWeekResponse, error) {
		if !healthy {
			return model.GitHubStarHistoryWeekResponse{}, errors.New("github down")
		}
		return weeklyPage(page, 1), nil
	}}
	handler := NewHistoryHandler(store, nil, time.Hour, 400, WithStarHistoryProvider(starHistory))
	handler.now = func() time.Time { return now }

	if _, err := handler.loadOfficialHistory(context.Background(), "owner", "repo"); err == nil {
		t.Fatal("expected the first attempt to fail")
	}
	if handler.backoff.failureCount("owner/repo") != 1 {
		t.Fatal("failure must be recorded")
	}
	healthy = true
	now = now.Add(backoffBase)
	if _, err := handler.loadOfficialHistory(context.Background(), "owner", "repo"); err != nil {
		t.Fatalf("recovered upstream must be usable: %v", err)
	}
	if got := handler.backoff.failureCount("owner/repo"); got != 0 {
		t.Fatalf("successful refresh must clear the backoff state, got %d failures", got)
	}
}

var _ provider.StarHistoryProvider = (*scriptedStarHistoryProvider)(nil)
