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
	if err := store.EnsureStatistics(ctx, Stats{Repositories: 1, EventDays: 2, WatchEvents: 3}); err != nil {
		t.Fatal(err)
	}
	metadata := RepositoryMetadata{RepoID: 1, FullName: "owner/repo", Visibility: "public", CurrentStars: 42, CheckedAt: time.Now().UTC()}
	if err := store.SaveMetadata(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	if gotMetadata, ok, err := store.Metadata(ctx, 1); err != nil || !ok || gotMetadata.FullName != metadata.FullName {
		t.Fatalf("unexpected metadata: %#v %v %v", gotMetadata, ok, err)
	}
	metadata.CurrentStars = 43
	if err := store.SaveMetadata(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	stats, err := store.OperationalStats(ctx)
	if err != nil || stats.Repositories != 1 || stats.EventDays != 2 || stats.WatchEvents != 3 || stats.MetadataEntries != 1 {
		t.Fatalf("unexpected constant-time statistics: %#v %v", stats, err)
	}
}

func TestApplyDeltaIsIdempotent(t *testing.T) {
	store, err := Open(t.TempDir() + "/history.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.SetActive(ctx, ActiveState{ModelVersion: "v1", ActiveWatermark: "2026-08-24", GeneratedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	applied, err := store.ApplyDelta(ctx, "2026-08-25", "2026-08-25", "abc", []DeltaRow{{RepoID: 1, EventDay: 20_000, EventCount: 2}})
	if err != nil || !applied {
		t.Fatalf("first apply failed: %v %v", applied, err)
	}
	applied, err = store.ApplyDelta(ctx, "2026-08-25", "2026-08-25", "abc", []DeltaRow{{RepoID: 1, EventDay: 20_000, EventCount: 2}})
	if err != nil || applied {
		t.Fatalf("replay should be a no-op: %v %v", applied, err)
	}
	stats, err := store.OperationalStats(ctx)
	if err != nil || stats.Repositories != 1 || stats.EventDays != 1 || stats.WatchEvents != 2 {
		t.Fatalf("delta statistics must be idempotent: %#v %v", stats, err)
	}
}
