package handler

import (
	"sync"
	"time"
)

// refreshBackoff 记录每个仓库下一次允许回源 GitHub 的时间。
//
// 为什么必须有它：GitHub 限流或抖动时，如果每个请求都重试，一次故障会被放大成
// 「请求数 × 回源数」的流量风暴 —— 而故障期间本来就是最不该加压的时候。指数退避
// 把单个仓库的重试压到 5→10→20→40→60 分钟，同时继续对外吐旧曲线。
//
// 只存在进程内存里：退避是"当下别打了"的短期判断，不需要跨重启保持；
// 重启后重新试一次是合理行为（那时 GitHub 可能已经恢复）。
type refreshBackoff struct {
	mu    sync.Mutex
	state map[string]backoffState
}

type backoffState struct {
	until    time.Time
	failures int
}

const (
	// backoffBase / backoffMax 是一次失败后的起始与封顶窗口。
	// 5 分钟起步：足够躲开 GitHub 的短时限流窗口，又不至于让恢复延迟太久。
	backoffBase = 5 * time.Minute
	backoffMax  = 60 * time.Minute
	// backoffStateLimit 限制内存占用：公开入口可以被任意 owner/repo 触发失败。
	backoffStateLimit = 4096
)

func newRefreshBackoff() *refreshBackoff {
	return &refreshBackoff{state: make(map[string]backoffState)}
}

// allow 报告现在是否允许为 key 回源；不允许时返回还需要等待多久。
func (b *refreshBackoff) allow(key string, now time.Time) (bool, time.Duration) {
	if b == nil {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	state, ok := b.state[key]
	if !ok || !now.Before(state.until) {
		return true, 0
	}
	return false, state.until.Sub(now)
}

// failure 记录一次失败，返回新的退避窗口（同时用作 stale 数据在内存里的存活时长）。
func (b *refreshBackoff) failure(key string, now time.Time) time.Duration {
	if b == nil {
		return backoffBase
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.state) >= backoffStateLimit {
		b.sweepLocked(now)
	}
	state := b.state[key]
	state.failures++
	window := backoffBase
	for i := 1; i < state.failures && window < backoffMax; i++ {
		window *= 2
	}
	if window > backoffMax {
		window = backoffMax
	}
	state.until = now.Add(window)
	b.state[key] = state
	return window
}

// success 清掉退避状态：回源成功说明外部依赖已经恢复。
func (b *refreshBackoff) success(key string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.state, key)
}

// failureCount 返回累计失败次数，供测试与排查使用。
func (b *refreshBackoff) failureCount(key string) int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state[key].failures
}

// sweepLocked 先清已经过期的条目；若仍然满，再整体重置。
//
// 淘汰策略故意选择"丢弃退避状态"而不是"拒绝记录"：丢掉退避的代价只是可能多打一次
// GitHub，而拒绝记录会让真正持续失败的仓库无限重试。
func (b *refreshBackoff) sweepLocked(now time.Time) {
	for key, state := range b.state {
		if !now.Before(state.until) {
			delete(b.state, key)
		}
	}
	if len(b.state) >= backoffStateLimit {
		b.state = make(map[string]backoffState)
	}
}
