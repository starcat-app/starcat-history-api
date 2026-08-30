package serving

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/series"
	_ "modernc.org/sqlite"
)

func TestRegistryInstallsSnapshotAndAppliesDelta(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	snapshotDirectory := filepath.Join(workspace, "snapshot")
	if err := os.Mkdir(snapshotDirectory, 0o750); err != nil {
		t.Fatal(err)
	}
	snapshotDB := filepath.Join(snapshotDirectory, snapshotDatabaseFile)
	store, err := Open(snapshotDB)
	if err != nil {
		t.Fatal(err)
	}
	events := []series.DayCount{{Day: 20_000, Count: 2}}
	payload, checksum, err := series.Encode(events)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSeries(ctx, RepositorySeries{RepoID: 7, CoverageStartDay: 20_000, CoverageEndDay: 20_000, EventTotal: 2, PointCount: 1, Encoding: series.Encoding, Series: payload, SourceWatermark: "2026-08-24", SeriesChecksum: checksum}); err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)
	if err := store.SetActive(ctx, ActiveState{ModelVersion: "watch-v1", ActiveWatermark: "2026-08-24", GeneratedAt: createdAt}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	snapshotManifest := SnapshotManifest{SchemaVersion: 1, Kind: "history_snapshot", ModelVersion: "watch-v1", SourceWatermark: "2026-08-24", CreatedAt: createdAt, Repositories: 1, EventDays: 1, WatchEvents: 2}
	snapshotZip := buildTestBundle(t, snapshotDirectory, snapshotManifestFile, snapshotManifest, snapshotDatabaseFile)

	registry, err := NewRegistry(filepath.Join(workspace, "registry"), filepath.Join(workspace, "bootstrap.sqlite"), 3)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if _, err := registry.InstallSnapshotZip(ctx, "watch-v1", bytes.NewReader(snapshotZip), true, 16<<20); err != nil {
		t.Fatal(err)
	}
	immutableSnapshot := filepath.Join(workspace, "registry", "versions", "watch-v1", snapshotDatabaseFile)
	immutableChecksum, err := fileChecksum(immutableSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	active, err := registry.Active(ctx)
	if err != nil || active.ModelVersion != "watch-v1" {
		t.Fatalf("unexpected active state: %#v %v", active, err)
	}

	deltaDirectory := filepath.Join(workspace, "delta")
	if err := os.Mkdir(deltaDirectory, 0o750); err != nil {
		t.Fatal(err)
	}
	deltaDB := filepath.Join(deltaDirectory, deltaDatabaseFile)
	database, err := sql.Open("sqlite", deltaDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE repo_star_daily_delta (repo_id INTEGER NOT NULL, event_day INTEGER NOT NULL, event_count INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO repo_star_daily_delta VALUES (7, 20001, 3)`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	deltaManifest := DeltaManifest{SchemaVersion: 1, Kind: "history_delta", DeltaID: "delta-20260825", FromWatermark: "2026-08-24", ToWatermark: "2026-08-25", CreatedAt: createdAt, Rows: 1, SourceChecksum: strings.Repeat("a", 64)}
	deltaZip := buildTestBundle(t, deltaDirectory, snapshotManifestFile, deltaManifest, deltaDatabaseFile)
	if _, applied, err := registry.InstallDeltaZip(ctx, "delta-20260825", bytes.NewReader(deltaZip), 16<<20); err != nil || !applied {
		t.Fatalf("apply delta failed: %v %v", applied, err)
	}
	if _, applied, err := registry.InstallDeltaZip(ctx, "delta-20260825", bytes.NewReader(deltaZip), 16<<20); err != nil || applied {
		t.Fatalf("delta replay should be a no-op: %v %v", applied, err)
	}
	updated, err := registry.Series(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := series.Decode(updated.Encoding, updated.Series, updated.SeriesChecksum, updated.PointCount)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 || decoded[1].Count != 3 {
		t.Fatalf("unexpected merged series: %#v", decoded)
	}
	afterDeltaChecksum, err := fileChecksum(immutableSnapshot)
	if err != nil || afterDeltaChecksum != immutableChecksum {
		t.Fatalf("delta must not mutate immutable snapshot: %s %s %v", immutableChecksum, afterDeltaChecksum, err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := NewRegistry(filepath.Join(workspace, "registry"), filepath.Join(workspace, "bootstrap.sqlite"), 3)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	restoredSeries, err := restored.Series(ctx, 7)
	if err != nil || restoredSeries.EventTotal != 5 {
		t.Fatalf("restart must restore delta-applied runtime: %#v %v", restoredSeries, err)
	}
}

func TestRegistryRetainsRollbackSnapshotAndPrunesCoveredDeltas(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	registryRoot := filepath.Join(workspace, "registry")
	registry, err := NewRegistry(registryRoot, filepath.Join(workspace, "bootstrap.sqlite"), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()

	for index := 1; index <= 4; index++ {
		version := fmt.Sprintf("watch-v%d", index)
		watermark := fmt.Sprintf("2026-08-%02d", 20+index)
		bundle := snapshotBundle(t, workspace, version, watermark, time.Date(2026, 8, 20+index, 0, 0, 0, 0, time.UTC))
		if _, err := registry.InstallSnapshotZip(ctx, version, bytes.NewReader(bundle), true, 16<<20); err != nil {
			t.Fatal(err)
		}
		if index == 1 {
			writeDeltaManifest(t, filepath.Join(registryRoot, "deltas", "covered"), "covered", "2026-08-21")
			writeDeltaManifest(t, filepath.Join(registryRoot, "deltas", "future"), "future", "2026-08-30")
		}
	}

	versions, err := os.ReadDir(filepath.Join(registryRoot, "versions"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range versions {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	if strings.Join(names, ",") != "watch-v3,watch-v4" {
		t.Fatalf("unexpected retained versions: %v", names)
	}
	if _, err := os.Stat(filepath.Join(registryRoot, "deltas", "covered")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("covered delta should be removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(registryRoot, "deltas", "future")); err != nil {
		t.Fatalf("future delta should remain: %v", err)
	}
	if err := registry.ActivateSnapshot("watch-v3"); err != nil {
		t.Fatalf("retained previous snapshot should support rollback: %v", err)
	}
}

func snapshotBundle(t *testing.T, workspace, version, watermark string, createdAt time.Time) []byte {
	t.Helper()
	directory := filepath.Join(workspace, version+"-source")
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(directory, snapshotDatabaseFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetActive(context.Background(), ActiveState{ModelVersion: version, ActiveWatermark: watermark, GeneratedAt: createdAt}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := SnapshotManifest{
		SchemaVersion: 1, Kind: "history_snapshot", ModelVersion: version,
		SourceWatermark: watermark, CreatedAt: createdAt,
	}
	return buildTestBundle(t, directory, snapshotManifestFile, manifest, snapshotDatabaseFile)
}

func writeDeltaManifest(t *testing.T, directory, deltaID, toWatermark string) {
	t.Helper()
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	manifest := DeltaManifest{
		SchemaVersion: 1, Kind: "history_delta", DeltaID: deltaID,
		FromWatermark: "2026-08-20", ToWatermark: toWatermark,
		CreatedAt: time.Now().UTC(), SourceChecksum: strings.Repeat("b", 64),
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, snapshotManifestFile), payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func buildTestBundle(t *testing.T, directory, manifestName string, manifest any, databaseName string) []byte {
	t.Helper()
	manifestPayload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, manifestName), manifestPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestChecksum, err := fileChecksum(filepath.Join(directory, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	databaseChecksum, err := fileChecksum(filepath.Join(directory, databaseName))
	if err != nil {
		t.Fatal(err)
	}
	checksumsPayload, err := json.Marshal(map[string]string{manifestName: manifestChecksum, databaseName: databaseChecksum})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, checksumsFile), checksumsPayload, 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	archive := zip.NewWriter(&output)
	names := []string{manifestName, checksumsFile, databaseName}
	sort.Strings(names)
	for _, name := range names {
		payload, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
