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
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	value := model.ErrorEnvelope{SchemaVersion: 1, Error: model.ErrorResponse{Code: code, Message: message, Details: details}}
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("[handler] encode error response: %v", err)
	}
}

// isUpstreamBusy 判断错误是否属于「上游暂时不可用，稍后重试」。
//
// 两类成因必须给客户端同一个答案：GitHub 限流了我们（ErrRateLimited）与我们自己的
// 出站闸门饱和（ErrBusy）。区分它们只对排查有意义，客户端能做的都是稍后重试。
func isUpstreamBusy(err error) bool {
	return errors.Is(err, provider.ErrRateLimited) || errors.Is(err, provider.ErrBusy)
}
