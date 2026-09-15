package handler

import (
	"testing"
	"time"
)

func TestBackoffWindowGrowsAndCaps(t *testing.T) {
	backoff := newRefreshBackoff()
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)

	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 60 * time.Minute, 60 * time.Minute}
	for index, expected := range want {
		if got := backoff.failure("owner/repo", now); got != expected {
			t.Fatalf("failure #%d: got window %s, want %s", index+1, got, expected)
		}
	}
}

func TestBackoffBlocksUntilWindowElapses(t *testing.T) {
	backoff := newRefreshBackoff()
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	window := backoff.failure("owner/repo", now)

	allowed, remaining := backoff.allow("owner/repo", now)
	if allowed {
		t.Fatal("must not allow a retry immediately after a failure")
	}
	if remaining <= 0 || remaining > window {
		t.Fatalf("unexpected remaining window %s (window %s)", remaining, window)
	}
	if allowed, _ := backoff.allow("owner/repo", now.Add(window)); !allowed {
		t.Fatal("retry must be allowed once the window elapses")
	}
	// 不同仓库互不影响。
	if allowed, _ := backoff.allow("other/repo", now); !allowed {
		t.Fatal("backoff must be per-repository")
	}
}

func TestBackoffSuccessClearsState(t *testing.T) {
	backoff := newRefreshBackoff()
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	backoff.failure("owner/repo", now)
	backoff.success("owner/repo")
	if allowed, _ := backoff.allow("owner/repo", now); !allowed {
		t.Fatal("success must clear the backoff window")
	}
	if backoff.failureCount("owner/repo") != 0 {
		t.Fatal("success must reset the failure counter")
	}
}

// 公开入口可以被任意 owner/repo 触发失败，退避表必须有界。
func TestBackoffStateStaysBounded(t *testing.T) {
	backoff := newRefreshBackoff()
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	for index := 0; index < backoffStateLimit+64; index++ {
		backoff.failure(string(rune('a'+index%26))+"/repo", now)
	}
	backoff.mu.Lock()
	size := len(backoff.state)
	backoff.mu.Unlock()
	if size > backoffStateLimit {
		t.Fatalf("backoff state must stay bounded, got %d entries", size)
	}
}

// 过期条目先被清掉，只有"全是活跃条目"时才会整体重置 —— 重置的代价只是多打一次
// GitHub，而拒绝记录会让持续失败的仓库无限重试。
func TestBackoffSweepDropsExpiredEntries(t *testing.T) {
	backoff := newRefreshBackoff()
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	for index := 0; index < backoffStateLimit; index++ {
		backoff.failure(string(rune('a'+index%26))+string(rune('a'+index/26))+"/repo", now)
	}
	// 所有条目都过期之后再压一条：应清掉旧的而不是无限增长。
	backoff.failure("fresh/repo", now.Add(2*time.Hour))
	backoff.mu.Lock()
	size := len(backoff.state)
	backoff.mu.Unlock()
	if size != 1 {
		t.Fatalf("expired entries must be swept, got %d entries", size)
	}
}
