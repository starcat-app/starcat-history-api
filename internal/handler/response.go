package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/starcat-app/starcat-history-api/internal/model"
	"github.com/starcat-app/starcat-history-api/internal/provider"
)

func writeJSON[T any](w http.ResponseWriter, status int, data T) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(model.DataEnvelope[T]{SchemaVersion: 1, Data: data}); err != nil {
		log.Printf("[handler] encode success response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string, details any) {
	// 错误响应必须显式声明缓存策略，理由见 writeErrorCacheControl。
	w.Header().Set("Cache-Control", writeErrorCacheControl(status))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	value := model.ErrorEnvelope{SchemaVersion: 1, Error: model.ErrorResponse{Code: code, Message: message, Details: details}}
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("[handler] encode error response: %v", err)
	}
}

// writeErrorCacheControl 决定错误响应的缓存策略。
//
// 背景：README 星标历史卡片经 GitHub Camo（Fastly）代理，共享缓存对上游错误的
// 处理跟随响应头。此前错误响应不带 Cache-Control，瞬时 429/503（回源 GitHub 被
// 限流）可能被 Camo 按默认 TTL 固化，源站恢复后 README 卡片仍裂着——表现为
// "同一批 URL 里有的裂有的好"的随机差异（浏览器/边缘只对部分 URL 恰好缓存过
// 正常副本）。
//
// 策略：
//   - 404（仓库不存在 / 非公开）：源站负缓存已挡住刷量，给共享缓存 5 分钟
//     短存，省掉 Camo 对不存在仓库的反复回源；
//   - 其余错误（400 请求非法、429 限流、5xx 故障）一律 no-store：瞬时故障
//     不进任何缓存，浏览器下一次加载就是一次干净的重试。
func writeErrorCacheControl(status int) string {
	if status == http.StatusNotFound {
		return "public, max-age=300"
	}
	return "no-store"
}

// isUpstreamBusy 判断错误是否属于「上游暂时不可用，稍后重试」。
//
// 两类成因必须给客户端同一个答案：GitHub 限流了我们（ErrRateLimited）与我们自己的
// 出站闸门饱和（ErrBusy）。区分它们只对排查有意义，客户端能做的都是稍后重试。
func isUpstreamBusy(err error) bool {
	return errors.Is(err, provider.ErrRateLimited) || errors.Is(err, provider.ErrBusy)
}
