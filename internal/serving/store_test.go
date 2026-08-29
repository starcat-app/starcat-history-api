package serving

import (
	"context"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	store, err := Open(t.TempDir() + "/history.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	value := RepositorySeries{RepoID: 1, CoverageStartDay: 17_000, CoverageEndDay: 17_001, EventTotal: 3, PointCount: 2, Encoding: "delta-uvarint-v1", Series: []byte{1, 2}, SourceWatermark: "2026-08-25", SeriesChecksum: "sum"}
	if err := store.UpsertSeries(ctx, value); err != nil {
		t.Fatal(err)
	}
	got, err := store.Series(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceWatermark != value.SourceWatermark || got.EventTotal != 3 {
		t.Fatalf("unexpected series: %#v", got)
	}
	metadata := RepositoryMetadata{RepoID: 1, FullName: "owner/repo", Visibility: "public", CurrentStars: 42, CheckedAt: time.Now().UTC()}
	if err := store.SaveMetadata(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	if gotMetadata, ok, err := store.Metadata(ctx, 1); err != nil || !ok || gotMetadata.FullName != metadata.FullName {
		t.Fatalf("unexpected metadata: %#v %v %v", gotMetadata, ok, err)
	}
}
