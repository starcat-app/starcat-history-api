// Package model 定义 Star History API 的稳定传输对象。
package model

import "time"

// GitHubStarHistoryWeek 是 GitHub 官方 stargazers/history 的一周数据。
// Week 是该周周日的 Unix 时间戳，Days 按周日到周六存储每日新增 Star 数。
type GitHubStarHistoryWeek struct {
	Week  int64 `json:"week"`
	Total int   `json:"total"`
	Days  []int `json:"days"`
}

// GitHubStarHistoryCache 是官方周数据在内存和 SQLite 中共用的缓存载荷。
// 当前 Star 数不放进载荷，因为它来自仓库 metadata，且每次重建都需要以最新值校准。
type GitHubStarHistoryCache struct {
	Weeks                  []GitHubStarHistoryWeek `json:"weeks"`
	ResponseETag           string                  `json:"response_etag,omitempty"`
	FetchedAt              time.Time               `json:"fetched_at"`
	FullHistoryValidatedAt time.Time               `json:"full_history_validated_at"`
}

// GitHubStarHistoryWeekResponse 是单页官方接口响应及其 HTTP 缓存信息。
type GitHubStarHistoryWeekResponse struct {
	Weeks        []GitHubStarHistoryWeek
	ResponseETag string
	NotModified  bool
}

// HistoryRange 是客户端支持的历史窗口。
type HistoryRange string

const (
	HistoryRangeThreeMonths HistoryRange = "3m"
	HistoryRangeOneYear     HistoryRange = "1y"
	HistoryRangeAll         HistoryRange = "all"
)

// Valid 判断范围是否属于 v1 契约。
func (r HistoryRange) Valid() bool {
	switch r {
	case HistoryRangeThreeMonths, HistoryRangeOneYear, HistoryRangeAll:
		return true
	default:
		return false
	}
}

// HistoryPoint 是 Starcat 图表直接消费的单个累计点。
type HistoryPoint struct {
	Date      string `json:"date"`
	Count     int    `json:"count"`
	Source    string `json:"source"`
	Precision string `json:"precision"`
}

// HistoryResponse 保持与现有 Discovery Star History v1 完全兼容。
// ModelVersion 和 ActiveWatermark 是可选扩展字段，旧客户端会自动忽略。
type HistoryResponse struct {
	RepoID          int64          `json:"repo_id"`
	FullName        string         `json:"full_name"`
	CurrentStars    int            `json:"current_stars"`
	Range           HistoryRange   `json:"range"`
	CoverageStart   string         `json:"coverage_start,omitempty"`
	GeneratedAt     time.Time      `json:"generated_at"`
	Points          []HistoryPoint `json:"points"`
	ModelVersion    string         `json:"model_version,omitempty"`
	ActiveWatermark string         `json:"active_watermark,omitempty"`
}

// HistoryEvent 是单日 WatchEvent 计数，尚未校准成 Star 曲线。
// Count 是当日事件数，不是累计星标。
type HistoryEvent struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// HistoryEventsResponse 专供 Starcat：只返回原始事件，由客户端用本地 stars_count 校准。
type HistoryEventsResponse struct {
	RepoID          int64          `json:"repo_id"`
	FullName        string         `json:"full_name"`
	CoverageStart   string         `json:"coverage_start,omitempty"`
	CoverageEnd     string         `json:"coverage_end,omitempty"`
	EventTotal      uint64         `json:"event_total"`
	GeneratedAt     time.Time      `json:"generated_at"`
	Events          []HistoryEvent `json:"events"`
	ModelVersion    string         `json:"model_version,omitempty"`
	ActiveWatermark string         `json:"active_watermark,omitempty"`
}

// DataEnvelope 是所有成功响应的统一外壳。
type DataEnvelope[T any] struct {
	SchemaVersion int `json:"schema_version"`
	Data          T   `json:"data"`
}

// ErrorResponse 是机器可判断的错误信息。
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// ErrorEnvelope 是所有失败响应的统一外壳。
type ErrorEnvelope struct {
	SchemaVersion int           `json:"schema_version"`
	Error         ErrorResponse `json:"error"`
}
