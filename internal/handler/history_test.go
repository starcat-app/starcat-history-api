package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/model"
	"github.com/starcat-app/starcat-history-api/internal/series"
	"github.com/starcat-app/starcat-history-api/internal/serving"
)

type fakeMetadataProvider struct{ value serving.RepositoryMetadata }

func (p fakeMetadataProvider) Fetch(context.Context, string, string) (serving.RepositoryMetadata, error) {
	return p.value, nil
}

func TestHistoryHandlerReturnsCompatibleEnvelopeAndETag(t *testing.T) {
	store, err := serving.Open(t.TempDir() + "/history.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events := []series.DayCount{{Day: series.DayFromTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), Count: 1}, {Day: series.DayFromTime(time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)), Count: 3}}
	payload, checksum, err := series.Encode(events)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.UpsertSeries(ctx, serving.RepositorySeries{RepoID: 42, CoverageStartDay: events[0].Day, CoverageEndDay: events[1].Day, EventTotal: 4, PointCount: 2, Encoding: series.Encoding, Series: payload, SourceWatermark: "2026-08-25", SeriesChecksum: checksum}); err != nil {
		t.Fatal(err)
	}
	generatedAt := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	if err := store.SetActive(ctx, serving.ActiveState{ModelVersion: "watch-v1", ActiveWatermark: "2026-08-25", GeneratedAt: generatedAt}); err != nil {
		t.Fatal(err)
	}
	metadata := serving.RepositoryMetadata{RepoID: 42, FullName: "owner/repo", Visibility: "public", CurrentStars: 100, CheckedAt: generatedAt}
	handler := NewHistoryHandler(store, fakeMetadataProvider{metadata}, time.Hour, 400)
	handler.now = func() time.Time { return generatedAt }
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

	conditional := httptest.NewRequest(http.MethodGet, "/api/v1/repos/owner/repo/star-history?repo_id=42&range=all", nil)
	conditional.Header.Set("If-None-Match", response.Header().Get("ETag"))
	notModified := httptest.NewRecorder()
	mux.ServeHTTP(notModified, conditional)
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("unexpected conditional status %d", notModified.Code)
	}
}
