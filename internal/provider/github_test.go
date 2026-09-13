package provider

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

const repoPayload = `{"id":1,"full_name":"o/r","private":false,"visibility":"public","stargazers_count":1,"created_at":"2026-01-01T00:00:00Z"}`

func TestNewGitHubProviderSplitsTokenPool(t *testing.T) {
	provider := NewGitHubProvider("", " alpha ,  ,beta ,", nil)
	if provider.pool.Count() != 2 {
		t.Fatalf("expected 2 tokens after trimming empty entries, got %d", provider.pool.Count())
	}
}

// 未配置 token 时池为空，请求必须匿名（不携带 Authorization）。
func TestEmptyPoolSendsAnonymousRequests(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(repoPayload))
	}))
	defer server.Close()

	provider := NewGitHubProvider(server.URL, " , ", server.Client())
	if _, err := provider.Fetch(t.Context(), "o", "r"); err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	if authorization != "" {
		t.Fatalf("anonymous request must not carry Authorization, got %q", authorization)
	}
}

// 池内 token 在未知额度时随机选取，但必须始终来自池内。
func TestRequestsUsePooledTokens(t *testing.T) {
	var authorizations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(repoPayload))
	}))
	defer server.Close()

	provider := NewGitHubProvider(server.URL, "a,b", server.Client())
	for i := 0; i < 4; i++ {
		if _, err := provider.Fetch(t.Context(), "o", "r"); err != nil {
			t.Fatalf("Fetch %d failed: %v", i, err)
		}
	}
	for i, authorization := range authorizations {
		if authorization != "Bearer a" && authorization != "Bearer b" {
			t.Fatalf("request %d got unexpected authorization %q", i, authorization)
		}
	}
}

// 403 + Retry-After 后该 token 被临时禁用：单 token 池的下一次请求直接返回
// ErrRateLimited，而不是继续打爆被限流的 token。
func TestRateLimitedTokenIsDisabled(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		t.Fatalf("second request should not reach GitHub after token was disabled")
	}))
	defer server.Close()

	provider := NewGitHubProvider(server.URL, "only", server.Client())
	if _, err := provider.Fetch(t.Context(), "o", "r"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("first request should report rate limited, got %v", err)
	}
	if _, err := provider.Fetch(t.Context(), "o", "r"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second request should short-circuit as rate limited, got %v", err)
	}
	if requests != 1 {
		t.Fatalf("expected exactly 1 outbound request, got %d", requests)
	}
}
