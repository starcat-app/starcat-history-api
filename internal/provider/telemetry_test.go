package provider

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/starcat-app/starcat-history-api/internal/telemetry"
)

// 「减少 GitHub 调用」这件事唯一可信的验收依据是真实出站请求数，所以计数必须挂在
// provider 真正发请求的位置，而不是 handler 的缓存判断上。
func TestTelemetryCountsOutboundGitHubCalls(t *testing.T) {
	registry := telemetry.NewRegistry()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/repos/o/r/stargazers/history" {
			_, _ = w.Write([]byte(`[{"w":1725696000,"total":3,"days":[1,0,2,0,0,0,0]}]`))
			return
		}
		_, _ = w.Write([]byte(repoPayload))
	}))
	defer server.Close()

	provider := NewGitHubProvider(server.URL, "token", server.Client()).WithTelemetry(registry)
	if _, err := provider.Fetch(t.Context(), "o", "r"); err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	if _, err := provider.StarHistory(t.Context(), "o", "r", 1, 30, ""); err != nil {
		t.Fatalf("StarHistory failed: %v", err)
	}

	snapshot := registry.Snapshot()
	if snapshot.GitHubMetadataRequests != 1 {
		t.Fatalf("expected 1 metadata request, got %d", snapshot.GitHubMetadataRequests)
	}
	if snapshot.GitHubHistoryRequests != 1 {
		t.Fatalf("expected 1 history request, got %d", snapshot.GitHubHistoryRequests)
	}
	// repoPayload 没有 owner.avatar_url：没有头像就没有出站请求，计数必须保持 0，
	// 否则「头像省了多少调用」这件事就没法用同一组数字衡量。
	if snapshot.GitHubAvatarRequests != 0 {
		t.Fatalf("repo without avatar must not produce an avatar request, got %d", snapshot.GitHubAvatarRequests)
	}
}

func TestTelemetryCountsRateLimitedTokens(t *testing.T) {
	registry := telemetry.NewRegistry()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	provider := NewGitHubProvider(server.URL, "only", server.Client()).WithTelemetry(registry)
	if _, err := provider.Fetch(t.Context(), "o", "r"); err == nil {
		t.Fatal("expected rate limited error")
	}
	if got := registry.Snapshot().GitHubRateLimited; got != 1 {
		t.Fatalf("expected 1 rate limited event, got %d", got)
	}
}
