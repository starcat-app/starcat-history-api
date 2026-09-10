package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/model"
	"github.com/starcat-app/starcat-history-api/internal/series"
	"github.com/starcat-app/starcat-history-api/internal/serving"
)

type fakeMetadataProvider struct {
	value serving.RepositoryMetadata
	calls atomic.Int64
}

func (p *fakeMetadataProvider) Fetch(context.Context, string, string) (serving.RepositoryMetadata, error) {
	p.calls.Add(1)
	return p.value, nil
}

func seedHistoryFixture(t *testing.T) (*serving.Store, *HistoryHandler, *fakeMetadataProvider, time.Time) {
	t.Helper()
	store, err := serving.Open(t.TempDir() + "/history.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	events := []series.DayCount{
		{Day: series.DayFromTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), Count: 1},
		{Day: series.DayFromTime(time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)), Count: 3},
	}
	payload, checksum, err := series.Encode(events)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.UpsertSeries(ctx, serving.RepositorySeries{
		RepoID: 42, CoverageStartDay: events[0].Day, CoverageEndDay: events[1].Day,
		EventTotal: 4, PointCount: 2, Encoding: series.Encoding, Series: payload,
		SourceWatermark: "2026-08-25", SeriesChecksum: checksum,
	}); err != nil {
		t.Fatal(err)
	}
	generatedAt := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	if err := store.SetActive(ctx, serving.ActiveState{
		ModelVersion: "watch-v1", ActiveWatermark: "2026-08-25", GeneratedAt: generatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := serving.RepositoryMetadata{
		RepoID: 42, FullName: "owner/repo", Visibility: "public", CurrentStars: 100, CheckedAt: generatedAt,
		Description: "A repository", Language: "Go", Topics: []string{"history"},
		CreatedAt: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
	}
	provider := &fakeMetadataProvider{value: metadata}
	handler := NewHistoryHandler(store, provider, time.Hour, 400)
	handler.now = func() time.Time { return generatedAt }
	return store, handler, provider, generatedAt
}

func TestHistoryHandlerReturnsCompatibleEnvelopeAndETag(t *testing.T) {
	_, handler, provider, _ := seedHistoryFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/star-history", handler.HandleStarHistory)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/owner/repo/star-history?repo_id=42&range=all", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
	}
	var envelope model.DataEnvelope[model.HistoryResponse]
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.CurrentStars != 100 || len(envelope.Data.Points) != 2 || envelope.Data.Points[1].Count != 100 {
		t.Fatalf("unexpected response: %#v", envelope.Data)
	}
	if response.Header().Get("ETag") == "" {
		t.Fatal("missing ETag")
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("expected one GitHub metadata fetch, got %d", provider.calls.Load())
	}

	conditional := httptest.NewRequest(http.MethodGet, "/api/v1/repos/owner/repo/star-history?repo_id=42&range=all", nil)
	conditional.Header.Set("If-None-Match", response.Header().Get("ETag"))
	notModified := httptest.NewRecorder()
	mux.ServeHTTP(notModified, conditional)
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("unexpected conditional status %d", notModified.Code)
	}
}

func TestHistoryHandlerUsesProvidedCurrentStarsWithoutGitHub(t *testing.T) {
	_, handler, provider, _ := seedHistoryFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/star-history", handler.HandleStarHistory)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/owner/repo/star-history?repo_id=42&range=all&current_stars=200", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
	}
	var envelope model.DataEnvelope[model.HistoryResponse]
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.CurrentStars != 200 || envelope.Data.Points[1].Count != 200 {
		t.Fatalf("expected client-provided current_stars calibration: %#v", envelope.Data)
	}
	if provider.calls.Load() != 0 {
		t.Fatalf("current_stars must skip GitHub metadata, got %d calls", provider.calls.Load())
	}
}

func TestHistoryHandlerRejectsInvalidCurrentStars(t *testing.T) {
	_, handler, provider, _ := seedHistoryFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/star-history", handler.HandleStarHistory)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/owner/repo/star-history?repo_id=42&range=all&current_stars=-1", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
	}
	if provider.calls.Load() != 0 {
		t.Fatalf("invalid current_stars must not hit GitHub, got %d calls", provider.calls.Load())
	}
}

func TestHistoryEventsHandlerReturnsRawDailyCounts(t *testing.T) {
	_, handler, provider, _ := seedHistoryFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/star-history/events", handler.HandleStarHistoryEvents)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/owner/repo/star-history/events?repo_id=42", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
	}
	var envelope model.DataEnvelope[model.HistoryEventsResponse]
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.RepoID != 42 || envelope.Data.EventTotal != 4 || len(envelope.Data.Events) != 2 {
		t.Fatalf("unexpected events payload: %#v", envelope.Data)
	}
	if envelope.Data.Events[0].Date != "2026-01-01" || envelope.Data.Events[0].Count != 1 {
		t.Fatalf("unexpected first event: %#v", envelope.Data.Events[0])
	}
	if envelope.Data.Events[1].Date != "2026-08-25" || envelope.Data.Events[1].Count != 3 {
		t.Fatalf("unexpected last event: %#v", envelope.Data.Events[1])
	}
	if provider.calls.Load() != 0 {
		t.Fatalf("events endpoint must not hit GitHub, got %d calls", provider.calls.Load())
	}

	conditional := httptest.NewRequest(http.MethodGet, "/api/v1/repos/owner/repo/star-history/events?repo_id=42", nil)
	conditional.Header.Set("If-None-Match", response.Header().Get("ETag"))
	notModified := httptest.NewRecorder()
	mux.ServeHTTP(notModified, conditional)
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("unexpected conditional status %d", notModified.Code)
	}
}
