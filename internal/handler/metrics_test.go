package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/model"
	"github.com/starcat-app/starcat-history-api/internal/provider"
	"github.com/starcat-app/starcat-history-api/internal/serving"
	"github.com/starcat-app/starcat-history-api/internal/telemetry"
)

// 命中与回源必须能被分开读出：优化「减少 GitHub 调用」时，唯一可信的验收依据
// 就是「同一个仓库连打 N 次，GitHub 调用数有没有涨」。
func TestOfficialHistoryReportsCacheHitsAndMisses(t *testing.T) {
	registry := telemetry.NewRegistry()
	store, err := serving.Open(t.TempDir() + "/history.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	metadata := serving.RepositoryMetadata{
		RepoID: 42, FullName: "owner/repo", Visibility: "public", CurrentStars: 100, CheckedAt: now,
	}
	if err := store.SaveMetadata(context.Background(), metadata); err != nil {
		t.Fatal(err)
	}
	starHistory := &fakeStarHistoryProvider{page: func(page int, _ string) model.GitHubStarHistoryWeekResponse {
		if page == 1 {
			return model.GitHubStarHistoryWeekResponse{
				Weeks: []model.GitHubStarHistoryWeek{
					{Week: 1_725_696_000, Total: 3, Days: []int{1, 0, 2, 0, 0, 0, 0}},
					{Week: 1_725_091_200, Total: 1, Days: []int{0, 1, 0, 0, 0, 0, 0}},
				}, ResponseETag: `"history-v1"`,
			}
		}
		return model.GitHubStarHistoryWeekResponse{}
	}}
	handler := NewHistoryHandler(
		store, &fakeMetadataProvider{value: metadata}, time.Hour, 400,
		WithStarHistoryProvider(starHistory), WithTelemetry(registry),
	)
	handler.now = func() time.Time { return now }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/star-history", handler.HandleStarHistory)

	call := func() {
		t.Helper()
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/repos/owner/repo/star-history?repo_id=42&range=all", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
		}
	}

	call()
	first := registry.Snapshot()
	if first.GitHubMetadataRequests != 0 {
		t.Fatalf("cached metadata must not hit github, got %d calls", first.GitHubMetadataRequests)
	}
	if first.MetadataCacheHits != 1 || first.MetadataCacheMisses != 0 {
		t.Fatalf("expected one metadata cache hit, got %+v", first)
	}
	if first.HistoryCacheMisses != 1 || first.HistoryCacheHits != 0 {
		t.Fatalf("cold history must be reported as one miss, got %+v", first)
	}
	// 冷启动的分页次数由 fixture 形状决定，这里只守住「真的去取了」，不固定页数。
	coldPages := starHistory.pages.Load()
	if coldPages < 1 {
		t.Fatalf("cold history must reach the provider at least once, got %d", coldPages)
	}

	call()
	second := registry.Snapshot()
	if starHistory.pages.Load() != coldPages {
		t.Fatalf("warm request must not reach the provider again: before=%d after=%d",
			coldPages, starHistory.pages.Load())
	}
	if second.HistoryCacheHits != 1 || second.HistoryCacheMisses != 1 {
		t.Fatalf("warm request must be a cache hit, got %+v", second)
	}
	if second.MetadataCacheHits != 2 || second.MetadataCacheMisses != 0 {
		t.Fatalf("warm request must stay on the metadata cache, got %+v", second)
	}
}

// metadata 未命中时既要计数，也不能因为计数改变原有行为（仍要回源并落库）。
func TestMetadataCacheMissCountsAndStillFetches(t *testing.T) {
	registry := telemetry.NewRegistry()
	store, err := serving.Open(t.TempDir() + "/history.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	metadataProvider := &fakeMetadataProvider{value: serving.RepositoryMetadata{
		RepoID: 42, FullName: "owner/repo", Visibility: "public", CurrentStars: 100, CheckedAt: now,
	}}
	handler := NewHistoryHandler(store, metadataProvider, time.Hour, 400, WithTelemetry(registry))
	handler.now = func() time.Time { return now }

	if _, err := handler.resolveMetadataCached(context.Background(), 0, "owner", "repo"); err != nil {
		t.Fatal(err)
	}
	snapshot := registry.Snapshot()
	if snapshot.MetadataCacheMisses != 1 || snapshot.MetadataCacheHits != 0 {
		t.Fatalf("expected one metadata cache miss, got %+v", snapshot)
	}
	if metadataProvider.calls.Load() != 1 {
		t.Fatalf("expected one github metadata call, got %d", metadataProvider.calls.Load())
	}
	if _, found, err := store.MetadataByFullName(context.Background(), "owner/repo"); err != nil || !found {
		t.Fatalf("metadata must still be persisted: found=%v err=%v", found, err)
	}
}

func TestHandleServiceMetricsExposesSnapshotAndReset(t *testing.T) {
	registry := telemetry.NewRegistry()
	registry.MetadataCacheMiss()
	mux := http.NewServeMux()
	mux.Handle("GET /internal/metrics/service", HandleServiceMetrics(registry))
	mux.Handle("POST /internal/metrics/service/reset", HandleServiceMetricsReset(registry))

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/internal/metrics/service", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status %d", response.Code)
	}
	var payload struct {
		SchemaVersion int                `json:"schema_version"`
		Data          telemetry.Snapshot `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode metrics response: %v", err)
	}
	if payload.SchemaVersion != 1 || payload.Data.MetadataCacheMisses != 1 {
		t.Fatalf("unexpected metrics payload: %+v", payload)
	}

	reset := httptest.NewRecorder()
	mux.ServeHTTP(reset, httptest.NewRequest(http.MethodPost, "/internal/metrics/service/reset", nil))
	if reset.Code != http.StatusOK {
		t.Fatalf("unexpected reset status %d", reset.Code)
	}
	if got := registry.Snapshot(); got != (telemetry.Snapshot{}) {
		t.Fatalf("reset endpoint left counters behind: %+v", got)
	}
}

// 未装配埋点时端点必须返回全零而不是 500：调用方只关心差值。
func TestHandleServiceMetricsToleratesMissingRegistry(t *testing.T) {
	response := httptest.NewRecorder()
	HandleServiceMetrics(nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/internal/metrics/service", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status %d", response.Code)
	}
}

var _ provider.StarHistoryProvider = (*fakeStarHistoryProvider)(nil)
