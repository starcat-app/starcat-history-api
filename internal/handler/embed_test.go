package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/starcat-app/starcat-history-api/internal/serving"
)

func TestHistoryEmbedHandlerReturnsSVGAndSupportsETag(t *testing.T) {
	_, handler, provider, _ := seedHistoryFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /embed/v1/repos/{owner}/{repo}/star-history.svg", handler.HandleStarHistoryEmbed)

	request := httptest.NewRequest(http.MethodGet, "/embed/v1/repos/OWNER/repo/star-history.svg?theme=dark&locale=zh", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "image/svg+xml; charset=utf-8" {
		t.Fatalf("unexpected content type %q", response.Header().Get("Content-Type"))
	}
	if response.Header().Get("Cache-Control") != "public, max-age=3600, s-maxage=86400, stale-while-revalidate=604800" {
		t.Fatalf("unexpected cache policy %q", response.Header().Get("Cache-Control"))
	}
	if !strings.Contains(response.Body.String(), "GitHub Star History") || !strings.Contains(response.Body.String(), "owner/repo") || !strings.Contains(response.Body.String(), "A repository") {
		t.Fatalf("unexpected SVG body: %s", response.Body.String())
	}
	etag := response.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("first embed request should resolve metadata once, got %d calls", provider.calls.Load())
	}

	conditional := httptest.NewRequest(http.MethodGet, "/embed/v1/repos/owner/repo/star-history.svg?theme=dark&locale=zh", nil)
	conditional.Header.Set("If-None-Match", etag)
	notModified := httptest.NewRecorder()
	mux.ServeHTTP(notModified, conditional)
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("unexpected conditional status %d", notModified.Code)
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("metadata cache should avoid a second GitHub request, got %d calls", provider.calls.Load())
	}
}

func TestHistoryEmbedHandlerRejectsPrivateMetadata(t *testing.T) {
	store, handler, _, generatedAt := seedHistoryFixture(t)
	if err := store.SaveMetadata(context.Background(), serving.RepositoryMetadata{
		RepoID: 42, FullName: "owner/repo", Visibility: "private", CurrentStars: 100, CheckedAt: generatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /embed/v1/repos/{owner}/{repo}/star-history.svg", handler.HandleStarHistoryEmbed)
	request := httptest.NewRequest(http.MethodGet, "/embed/v1/repos/owner/repo/star-history.svg", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("private repository must be hidden, got %d: %s", response.Code, response.Body.String())
	}
}

func TestHistoryEmbedHandlerRejectsUnapprovedOptions(t *testing.T) {
	_, handler, provider, _ := seedHistoryFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /embed/v1/repos/{owner}/{repo}/star-history.svg", handler.HandleStarHistoryEmbed)
	request := httptest.NewRequest(http.MethodGet, "/embed/v1/repos/owner/repo/star-history.svg?theme=custom&locale=en", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unapproved options must be rejected, got %d: %s", response.Code, response.Body.String())
	}
	if provider.calls.Load() != 0 {
		t.Fatalf("invalid options must not resolve metadata, got %d calls", provider.calls.Load())
	}
}
