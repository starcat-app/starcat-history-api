package provider

import (
	"context"
	"errors"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/telemetry"
)

// ErrBusy 表示全局出站闸门已饱和，这次没有拿到槽位。
//
// 与 ErrRateLimited 分开：两者对客户端语义相同（稍后重试），但成因不同 ——
// ErrRateLimited 是 GitHub 限了我们，ErrBusy 是我们自己限了自己。混在一起会让
// 「到底谁在限流」这个问题永远查不清。
var ErrBusy = errors.New("github call limiter is saturated")

// defaultCallAcquireTimeout 是排队等槽位的最长时间。
//
// 必须比客户端超时（8s）短：把时间花在排队上，不如尽快失败并让上层走 stale 兜底。
const defaultCallAcquireTimeout = 5 * time.Second

// callLimiter 给所有出站 GitHub 调用加一道全局闸门。
//
// 为什么需要：多个仓库的 README 同时首次被访问时，冷启动会在瞬间打出几十个请求
// （单仓库就有 20+ 页分页）。GitHub 的二级限流正是按"并发突发"判定的，一旦触发，
// 整池 token 都会在几分钟内不可用 —— 对公开图片入口来说这是最糟的故障形态。
//
// 代价是排队：饱和时请求最多等 defaultCallAcquireTimeout，超时即返回 ErrBusy，
// 由上层回退到缓存或 stale 数据。宁可少刷一次新数据，也不要把请求堆在队列里烂掉。
type callLimiter struct {
	slots          chan struct{}
	acquireTimeout time.Duration
	telemetry      *telemetry.Registry
}

func newCallLimiter(maxConcurrency int, acquireTimeout time.Duration, registry *telemetry.Registry) *callLimiter {
	if maxConcurrency <= 0 {
		return nil
	}
	if acquireTimeout <= 0 {
		acquireTimeout = defaultCallAcquireTimeout
	}
	return &callLimiter{
		slots:          make(chan struct{}, maxConcurrency),
		acquireTimeout: acquireTimeout,
		telemetry:      registry,
	}
}

// acquire 申请一个出站槽位；失败时返回 ErrBusy 或 ctx 的错误。
// 成功后必须调用返回的 release（用 defer）。
func (l *callLimiter) acquire(ctx context.Context) (func(), error) {
	if l == nil {
		return func() {}, nil
	}
	// 快路径：有空位就不要建 timer。
	select {
	case l.slots <- struct{}{}:
		return func() { <-l.slots }, nil
	default:
	}

	timer := time.NewTimer(l.acquireTimeout)
	defer timer.Stop()
	select {
	case l.slots <- struct{}{}:
		return func() { <-l.slots }, nil
	case <-timer.C:
		l.telemetry.LimiterTimeout()
		return nil, ErrBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
