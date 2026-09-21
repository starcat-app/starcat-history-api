package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/starcat-app/starcat-history-api/internal/model"
)

// 错误响应的 Cache-Control 是 README 卡片可用性契约的一部分：瞬时故障（429/5xx）
// 不能被 Camo / Fastly 固化，否则源站恢复后卡片仍裂着；404 允许短存以省回源。
func TestWriteErrorCacheControlPolicy(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusNotFound, "public, max-age=300"},
		{http.StatusBadRequest, "no-store"},
		{http.StatusTooManyRequests, "no-store"},
		{http.StatusInternalServerError, "no-store"},
		{http.StatusServiceUnavailable, "no-store"},
	}
	for _, tc := range cases {
		if got := writeErrorCacheControl(tc.status); got != tc.want {
			t.Errorf("status %d: cache policy = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestWriteErrorSetsSingleCacheControlAndEnvelope(t *testing.T) {
	response := httptest.NewRecorder()
	writeError(response, http.StatusTooManyRequests, "GITHUB_RATE_LIMITED", "GitHub metadata is temporarily unavailable.", nil)

	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("unexpected status %d", response.Code)
	}
	if values := response.Header().Values("Cache-Control"); len(values) != 1 || values[0] != "no-store" {
		t.Fatalf("429 must carry exactly one no-store Cache-Control, got %v", values)
	}
	if response.Header().Get("Retry-After") != "" {
		// Retry-After 由调用方按需追加；writeError 自身不写，避免覆盖。
		t.Fatalf("writeError must not set Retry-After itself")
	}
	var envelope struct {
		SchemaVersion int                 `json:"schema_version"`
		Error         model.ErrorResponse `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if envelope.Error.Code != "GITHUB_RATE_LIMITED" {
		t.Fatalf("unexpected error code %q", envelope.Error.Code)
	}
}

// 404 走短存分支：负缓存语义（仓库确实不可提供）允许共享缓存短暂保留。
func TestWriteErrorNotFoundUsesShortCache(t *testing.T) {
	response := httptest.NewRecorder()
	writeError(response, http.StatusNotFound, "REPOSITORY_NOT_FOUND", "Public repository history is not available.", nil)

	if values := response.Header().Values("Cache-Control"); len(values) != 1 || values[0] != "public, max-age=300" {
		t.Fatalf("404 must carry public short cache, got %v", values)
	}
}
