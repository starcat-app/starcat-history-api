package handler

import (
	"context"
	"net/http"

	"github.com/starcat-app/starcat-history-api/internal/serving"
)

// StatsProvider 是 Store 与 Registry 共同实现的最小统计接口。
type StatsProvider interface {
	OperationalStats(context.Context) (serving.Stats, error)
}

// HandlePing 返回客户端和运维脚本使用的稳定探针。
func HandlePing(service, version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"service": service, "version": version, "ok": true})
	})
}

// HandleStats 返回不包含本机文件路径的 Serving 规模统计。
func HandleStats(store StatsProvider) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stats, err := store.OperationalStats(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "STORE_ERROR", "Unable to read history statistics.", nil)
			return
		}
		writeJSON(w, http.StatusOK, stats)
	})
}
