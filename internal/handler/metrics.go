package handler

import (
	"net/http"

	"github.com/starcat-app/starcat-history-api/internal/telemetry"
)

// HandleServiceMetrics 暴露进程内计数指标：
// GET /internal/metrics/service
//
// 与 kitmetrics 的分工：那一侧负责按路由的耗时、状态码、响应字节并落 SQLite；
// 这里只回答「回源了几次 GitHub」「缓存命中几次」，用于压测取差值和线上排查。
// 端点必须挂在 API_KEYS 鉴权之后，计数里含缓存命中率，属于内部运维信息。
func HandleServiceMetrics(registry *telemetry.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// registry 为 nil（未装配埋点）时返回全零快照，而不是 500：
		// 调用方只关心差值，缺埋点应表现为「没有数据」而不是「端点坏了」。
		writeJSON(w, http.StatusOK, registry.Snapshot())
	})
}

// HandleServiceMetricsReset 归零计数：
// POST /internal/metrics/service/reset
//
// 压测脚本用它取单场景差值。用独立端点而不是 query 参数，避免 GET 的
// 缓存/预取语义意外把线上计数清掉。
func HandleServiceMetricsReset(registry *telemetry.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Use POST to reset service metrics.", nil)
			return
		}
		registry.Reset()
		writeJSON(w, http.StatusOK, registry.Snapshot())
	})
}
