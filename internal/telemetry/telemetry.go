// Package telemetry 提供进程内计数指标。
//
// 为什么单独做一套：kitmetrics 只记录「按路由的耗时 / 状态码 / 响应字节」，
// 回答不了「这次请求有没有回源 GitHub」「缓存有没有命中」这类问题。而 history-api
// 的对外成本几乎完全由回源次数决定（公开 README 图片入口会把每个请求放大成
// GitHub 调用），所以必须有一组能直接读出差值的计数器，否则优化前后无法对比。
//
// 设计约束：
//   - 只做计数，不做聚合与持久化；进程重启即归零（长期趋势仍由 kitmetrics 负责）。
//   - 所有方法都必须容忍 nil 接收者：provider / handler 的零值配置（单测）不该
//     为了埋点被迫构造一个 Registry。
//   - 计数不参与业务判断，任何一处埋点失败都不允许影响请求结果。
package telemetry

import "sync/atomic"

// Registry 是进程内计数器集合。零值不可用，必须用 NewRegistry 构造。
type Registry struct {
	githubMetadata    atomic.Int64
	githubHistory     atomic.Int64
	githubAvatar      atomic.Int64
	githubRateLimited atomic.Int64

	metadataCacheHits    atomic.Int64
	metadataCacheMisses  atomic.Int64
	metadataNegativeHits atomic.Int64

	historyCacheHits   atomic.Int64
	historyCacheMisses atomic.Int64
	historyStaleServed atomic.Int64
}

// NewRegistry 创建计数器集合。
func NewRegistry() *Registry { return &Registry{} }

// Snapshot 是一次读取的计数快照，字段均为「自上次 Reset 或进程启动以来」的累计值。
type Snapshot struct {
	GitHubMetadataRequests int64 `json:"github_metadata_requests"`
	GitHubHistoryRequests  int64 `json:"github_history_requests"`
	GitHubAvatarRequests   int64 `json:"github_avatar_requests"`
	GitHubRateLimited      int64 `json:"github_rate_limited"`

	MetadataCacheHits    int64 `json:"metadata_cache_hits"`
	MetadataCacheMisses  int64 `json:"metadata_cache_misses"`
	MetadataNegativeHits int64 `json:"metadata_negative_hits"`

	HistoryCacheHits   int64 `json:"history_cache_hits"`
	HistoryCacheMisses int64 `json:"history_cache_misses"`
	HistoryStaleServed int64 `json:"history_stale_served"`
}

// Snapshot 读取当前计数。
func (r *Registry) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	return Snapshot{
		GitHubMetadataRequests: r.githubMetadata.Load(),
		GitHubHistoryRequests:  r.githubHistory.Load(),
		GitHubAvatarRequests:   r.githubAvatar.Load(),
		GitHubRateLimited:      r.githubRateLimited.Load(),
		MetadataCacheHits:      r.metadataCacheHits.Load(),
		MetadataCacheMisses:    r.metadataCacheMisses.Load(),
		MetadataNegativeHits:   r.metadataNegativeHits.Load(),
		HistoryCacheHits:       r.historyCacheHits.Load(),
		HistoryCacheMisses:     r.historyCacheMisses.Load(),
		HistoryStaleServed:     r.historyStaleServed.Load(),
	}
}

// Reset 归零所有计数。
//
// 压测脚本用它取「某个场景区间内的差值」：先 Reset，再打流量，再读 Snapshot。
func (r *Registry) Reset() {
	if r == nil {
		return
	}
	for _, counter := range []*atomic.Int64{
		&r.githubMetadata, &r.githubHistory, &r.githubAvatar, &r.githubRateLimited,
		&r.metadataCacheHits, &r.metadataCacheMisses, &r.metadataNegativeHits,
		&r.historyCacheHits, &r.historyCacheMisses, &r.historyStaleServed,
	} {
		counter.Store(0)
	}
}

// MetadataRequested 记录一次 GitHub 仓库 metadata 回源。
func (r *Registry) MetadataRequested() {
	if r == nil {
		return
	}
	r.githubMetadata.Add(1)
}

// HistoryRequested 记录一次 GitHub 星标历史分页请求。
func (r *Registry) HistoryRequested() {
	if r == nil {
		return
	}
	r.githubHistory.Add(1)
}

// AvatarRequested 记录一次头像下载。
func (r *Registry) AvatarRequested() {
	if r == nil {
		return
	}
	r.githubAvatar.Add(1)
}

// RateLimited 记录一次被 GitHub 限流（token 被临时禁用）。
func (r *Registry) RateLimited() {
	if r == nil {
		return
	}
	r.githubRateLimited.Add(1)
}

// MetadataCacheHit 记录一次 metadata 命中持久缓存、未回源 GitHub。
func (r *Registry) MetadataCacheHit() {
	if r == nil {
		return
	}
	r.metadataCacheHits.Add(1)
}

// MetadataCacheMiss 记录一次 metadata 未命中、需要回源 GitHub。
func (r *Registry) MetadataCacheMiss() {
	if r == nil {
		return
	}
	r.metadataCacheMisses.Add(1)
}

// MetadataNegativeHit 记录一次命中负缓存（404 / 非公开仓库）。
func (r *Registry) MetadataNegativeHit() {
	if r == nil {
		return
	}
	r.metadataNegativeHits.Add(1)
}

// HistoryCacheHit 记录一次星标历史命中缓存（内存或 SQLite）。
func (r *Registry) HistoryCacheHit() {
	if r == nil {
		return
	}
	r.historyCacheHits.Add(1)
}

// HistoryCacheMiss 记录一次星标历史需要回源 GitHub。
func (r *Registry) HistoryCacheMiss() {
	if r == nil {
		return
	}
	r.historyCacheMisses.Add(1)
}

// HistoryStaleServed 记录一次回源失败后继续吐出旧曲线。
func (r *Registry) HistoryStaleServed() {
	if r == nil {
		return
	}
	r.historyStaleServed.Add(1)
}
