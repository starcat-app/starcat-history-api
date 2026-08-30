package serving

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

const (
	snapshotManifestFile = "manifest.json"
	checksumsFile        = "checksums.json"
	snapshotDatabaseFile = "history.sqlite"
	deltaDatabaseFile    = "history-delta.sqlite"
)

var (
	ErrInvalidBundle       = errors.New("invalid history bundle")
	ErrVersionConflict     = errors.New("history version already exists with different content")
	ErrWatermarkConflict   = errors.New("history watermark conflict")
	ErrInsufficientStorage = errors.New("insufficient history registry storage")
	identifierPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	checksumPattern        = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

const (
	installExpansionFactor  = int64(4)
	installFreeReserveBytes = int64(1 << 30)
)

// SnapshotManifest 是本地 Builder 和云端服务之间的快照契约。
type SnapshotManifest struct {
	SchemaVersion   int       `json:"schema_version"`
	Kind            string    `json:"kind"`
	ModelVersion    string    `json:"model_version"`
	SourceWatermark string    `json:"source_watermark"`
	CreatedAt       time.Time `json:"created_at"`
	Repositories    int64     `json:"repositories"`
	EventDays       int64     `json:"event_days"`
	WatchEvents     int64     `json:"watch_events"`
}

// DeltaManifest 描述相邻水位之间的一次日增量。
type DeltaManifest struct {
	SchemaVersion  int       `json:"schema_version"`
	Kind           string    `json:"kind"`
	DeltaID        string    `json:"delta_id"`
	FromWatermark  string    `json:"from_watermark"`
	ToWatermark    string    `json:"to_watermark"`
	CreatedAt      time.Time `json:"created_at"`
	Rows           int64     `json:"rows"`
	SourceChecksum string    `json:"source_checksum"`
}

type activePointer struct {
	ModelVersion string `json:"model_version"`
	StoreFile    string `json:"store_file,omitempty"`
}

// Registry 持有当前 Store，并负责快照的原子切换和增量串行应用。
// 查询拿读锁后只访问一个固定 Store，因此切换期间不会读到半个版本。
type Registry struct {
	root       string
	versions   string
	deltas     string
	runtime    string
	activeFile string
	publishMu  sync.Mutex
	mu         sync.RWMutex
	store      *Store
	// snapshotRetention 至少保留当前与上一个版本，保证激活失败或数据回归时可快速回滚。
	snapshotRetention int
}

// NewRegistry 恢复已激活快照；首次启动则使用 bootstrapStoreFile 创建空库。
func NewRegistry(root, bootstrapStoreFile string, snapshotRetention int) (*Registry, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("history registry directory is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if snapshotRetention < 2 {
		snapshotRetention = 2
	}
	registry := &Registry{
		root: absolute, versions: filepath.Join(absolute, "versions"),
		deltas: filepath.Join(absolute, "deltas"), runtime: filepath.Join(absolute, "runtime"),
		activeFile:        filepath.Join(absolute, "active.json"),
		snapshotRetention: snapshotRetention,
	}
	for _, directory := range []string{registry.versions, registry.deltas, registry.runtime} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return nil, err
		}
	}
	if payload, err := os.ReadFile(registry.activeFile); err == nil {
		var pointer activePointer
		if err := json.Unmarshal(payload, &pointer); err != nil || !identifierPattern.MatchString(pointer.ModelVersion) {
			return nil, fmt.Errorf("restore active history pointer: %w", ErrInvalidBundle)
		}
		directory := filepath.Join(registry.versions, pointer.ModelVersion)
		if _, err := verifySnapshot(directory, pointer.ModelVersion); err != nil {
			return nil, fmt.Errorf("restore active history snapshot: %w", err)
		}
		storeFile, err := registry.resolveRuntimeFile(pointer.StoreFile)
		if err != nil {
			return nil, err
		}
		var store *Store
		if storeFile == "" {
			// 兼容尚未写入 runtime 路径的预发布指针；迁移时复制而不是继续修改 Snapshot。
			store, pointer.StoreFile, err = registry.prepareRuntime(pointer.ModelVersion)
			if err == nil {
				err = registry.writeActivePointer(pointer)
			}
		} else {
			store, err = Open(storeFile)
		}
		if err != nil {
			return nil, err
		}
		state, err := store.Active(context.Background())
		if err != nil || state.ModelVersion != pointer.ModelVersion {
			store.Close()
			return nil, fmt.Errorf("restore active history runtime: %w", ErrInvalidBundle)
		}
		registry.store = store
		registry.cleanupRuntimes(pointer.StoreFile)
		return registry, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	store, err := Open(bootstrapStoreFile)
	if err != nil {
		return nil, err
	}
	registry.store = store
	return registry, nil
}

func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.store == nil {
		return nil
	}
	err := r.store.Close()
	r.store = nil
	return err
}

func (r *Registry) Series(ctx context.Context, repoID int64) (RepositorySeries, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store.Series(ctx, repoID)
}

func (r *Registry) Metadata(ctx context.Context, repoID int64) (RepositoryMetadata, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store.Metadata(ctx, repoID)
}

func (r *Registry) SaveMetadata(ctx context.Context, value RepositoryMetadata) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store.SaveMetadata(ctx, value)
}

func (r *Registry) Active(ctx context.Context) (ActiveState, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store.Active(ctx)
}

func (r *Registry) OperationalStats(ctx context.Context) (Stats, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store.OperationalStats(ctx)
}

// EnsureInstallCapacity 在读取大包前预留解压、runtime 副本和安全余量，避免上传到一半占满卷。
func (r *Registry) EnsureInstallCapacity(uploadBytes int64) error {
	if uploadBytes <= 0 {
		return nil
	}
	if uploadBytes > (int64(^uint64(0)>>1)-installFreeReserveBytes)/installExpansionFactor {
		return fmt.Errorf("%w: upload size overflow", ErrInsufficientStorage)
	}
	required := uploadBytes*installExpansionFactor + installFreeReserveBytes
	return r.ensureAvailable(required)
}

func (r *Registry) ensureAvailable(required int64) error {
	var status syscall.Statfs_t
	if err := syscall.Statfs(r.root, &status); err != nil {
		return err
	}
	available := int64(status.Bavail) * int64(status.Bsize)
	if available < required {
		return fmt.Errorf("%w: available=%d required=%d", ErrInsufficientStorage, available, required)
	}
	return nil
}

// InstallSnapshotZip 校验并不可变安装快照，activate=true 时原子切换查询 Store。
func (r *Registry) InstallSnapshotZip(ctx context.Context, version string, reader io.Reader, activate bool, maximumBytes int64) (SnapshotManifest, error) {
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	if !identifierPattern.MatchString(version) {
		return SnapshotManifest{}, fmt.Errorf("%w: invalid model version", ErrInvalidBundle)
	}
	staging, err := os.MkdirTemp(r.root, ".snapshot-staging-")
	if err != nil {
		return SnapshotManifest{}, err
	}
	defer os.RemoveAll(staging)
	extracted, checksums, err := extractAndVerifyZip(reader, staging, maximumBytes, []string{snapshotManifestFile, checksumsFile, snapshotDatabaseFile})
	if err != nil {
		return SnapshotManifest{}, err
	}
	manifest, err := verifySnapshot(extracted, version)
	if err != nil {
		return SnapshotManifest{}, err
	}
	destination := filepath.Join(r.versions, version)
	if err := installImmutableDirectory(extracted, destination, checksums); err != nil {
		return SnapshotManifest{}, err
	}
	if activate {
		if err := r.activate(version); err != nil {
			return SnapshotManifest{}, err
		}
	}
	return manifest, nil
}

// ActivateSnapshot 回滚或前滚到一个已经校验安装的版本。
func (r *Registry) ActivateSnapshot(version string) error {
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	if !identifierPattern.MatchString(version) {
		return fmt.Errorf("%w: invalid model version", ErrInvalidBundle)
	}
	return r.activate(version)
}

func (r *Registry) activate(version string) error {
	directory := filepath.Join(r.versions, version)
	if _, err := verifySnapshot(directory, version); err != nil {
		return err
	}
	newStore, runtimeFile, err := r.prepareRuntime(version)
	if err != nil {
		return err
	}
	state, err := newStore.Active(context.Background())
	if err != nil || state.ModelVersion != version {
		newStore.Close()
		os.Remove(filepath.Join(r.root, runtimeFile))
		return fmt.Errorf("%w: snapshot active state does not match manifest", ErrInvalidBundle)
	}
	previousVersion := r.currentPointerVersion()
	if err := r.writeActivePointer(activePointer{ModelVersion: version, StoreFile: runtimeFile}); err != nil {
		newStore.Close()
		os.Remove(filepath.Join(r.root, runtimeFile))
		return err
	}
	r.mu.Lock()
	oldStore := r.store
	r.store = newStore
	r.mu.Unlock()
	if oldStore != nil {
		if err := oldStore.Close(); err != nil {
			log.Printf("close previous history runtime failed: %v", err)
		}
	}
	r.cleanupRuntimes(runtimeFile)
	// 清理失败不回滚已完成的原子切换，否则调用方会误以为激活失败并重复发布。
	if err := r.cleanupArtifacts(version, previousVersion, state.ActiveWatermark); err != nil {
		log.Printf("history registry cleanup failed after activating %s: %v", version, err)
	}
	return nil
}

// prepareRuntime 从不可变 Snapshot 复制出独立可写库，Delta 永远不修改 Snapshot 本体。
func (r *Registry) prepareRuntime(version string) (*Store, string, error) {
	source := filepath.Join(r.versions, version, snapshotDatabaseFile)
	info, err := os.Stat(source)
	if err != nil {
		return nil, "", err
	}
	if err := r.ensureAvailable(info.Size() + installFreeReserveBytes); err != nil {
		return nil, "", err
	}
	name := fmt.Sprintf("%s-%d.sqlite", version, time.Now().UTC().UnixNano())
	relative := filepath.Join("runtime", name)
	target := filepath.Join(r.root, relative)
	temporary := target + ".tmp"
	if err := copyFile(source, temporary); err != nil {
		os.Remove(temporary)
		return nil, "", err
	}
	if err := os.Rename(temporary, target); err != nil {
		os.Remove(temporary)
		return nil, "", err
	}
	store, err := Open(target)
	if err != nil {
		os.Remove(target)
		return nil, "", err
	}
	return store, relative, nil
}

func (r *Registry) writeActivePointer(pointer activePointer) error {
	payload, err := json.Marshal(pointer)
	if err != nil {
		return err
	}
	temporary := r.activeFile + ".tmp"
	if err := os.WriteFile(temporary, payload, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, r.activeFile)
}

func (r *Registry) resolveRuntimeFile(relative string) (string, error) {
	if strings.TrimSpace(relative) == "" {
		return "", nil
	}
	cleaned := filepath.Clean(relative)
	if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("restore active history runtime: %w", ErrInvalidBundle)
	}
	absolute := filepath.Join(r.root, cleaned)
	if filepath.Dir(absolute) != r.runtime {
		return "", fmt.Errorf("restore active history runtime: %w", ErrInvalidBundle)
	}
	return absolute, nil
}

func (r *Registry) cleanupRuntimes(activeRelative string) {
	active, err := r.resolveRuntimeFile(activeRelative)
	if err != nil {
		return
	}
	entries, err := os.ReadDir(r.runtime)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(r.runtime, entry.Name())
		if entry.IsDir() || path == active {
			continue
		}
		if err := os.Remove(path); err != nil {
			log.Printf("remove stale history runtime %s failed: %v", path, err)
		}
	}
}

func copyFile(source, target string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func (r *Registry) currentPointerVersion() string {
	payload, err := os.ReadFile(r.activeFile)
	if err != nil {
		return ""
	}
	var pointer activePointer
	if json.Unmarshal(payload, &pointer) != nil || !identifierPattern.MatchString(pointer.ModelVersion) {
		return ""
	}
	return pointer.ModelVersion
}

// cleanupArtifacts 保留可回滚快照，并删除已被新快照水位覆盖的 Delta 包。
func (r *Registry) cleanupArtifacts(activeVersion, previousVersion, activeWatermark string) error {
	entries, err := os.ReadDir(r.versions)
	if err != nil {
		return err
	}
	type candidate struct {
		version   string
		createdAt time.Time
	}
	candidates := make([]candidate, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !identifierPattern.MatchString(entry.Name()) {
			continue
		}
		payload, err := os.ReadFile(filepath.Join(r.versions, entry.Name(), snapshotManifestFile))
		if err != nil {
			continue
		}
		var manifest SnapshotManifest
		if json.Unmarshal(payload, &manifest) != nil || manifest.ModelVersion != entry.Name() || manifest.CreatedAt.IsZero() {
			continue
		}
		candidates = append(candidates, candidate{version: entry.Name(), createdAt: manifest.CreatedAt})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].createdAt.After(candidates[j].createdAt) })
	keep := map[string]struct{}{activeVersion: {}}
	if previousVersion != "" {
		keep[previousVersion] = struct{}{}
	}
	for _, item := range candidates {
		if len(keep) >= r.snapshotRetention {
			break
		}
		keep[item.version] = struct{}{}
	}
	for _, item := range candidates {
		if _, retained := keep[item.version]; retained {
			continue
		}
		if err := os.RemoveAll(filepath.Join(r.versions, item.version)); err != nil {
			return err
		}
	}

	watermark, err := time.Parse("2006-01-02", activeWatermark)
	if err != nil {
		return err
	}
	deltas, err := os.ReadDir(r.deltas)
	if err != nil {
		return err
	}
	for _, entry := range deltas {
		if !entry.IsDir() || !identifierPattern.MatchString(entry.Name()) {
			continue
		}
		payload, err := os.ReadFile(filepath.Join(r.deltas, entry.Name(), snapshotManifestFile))
		if err != nil {
			continue
		}
		var manifest DeltaManifest
		if json.Unmarshal(payload, &manifest) != nil {
			continue
		}
		toWatermark, parseErr := time.Parse("2006-01-02", manifest.ToWatermark)
		if parseErr == nil && !toWatermark.After(watermark) {
			if err := os.RemoveAll(filepath.Join(r.deltas, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// InstallDeltaZip 校验日增量后应用到当前快照；相同 delta_id 可安全重放。
func (r *Registry) InstallDeltaZip(ctx context.Context, deltaID string, reader io.Reader, maximumBytes int64) (DeltaManifest, bool, error) {
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	if !identifierPattern.MatchString(deltaID) {
		return DeltaManifest{}, false, fmt.Errorf("%w: invalid delta id", ErrInvalidBundle)
	}
	// bootstrap STORE_FILE 仅用于本地只读查询。只有 Registry 管理的 Snapshot 才有
	// 独立 runtime 与可恢复指针，禁止 Delta 直接修改外部 bootstrap 文件。
	if r.currentPointerVersion() == "" {
		return DeltaManifest{}, false, fmt.Errorf("%w: no registry-managed snapshot is active", ErrWatermarkConflict)
	}
	staging, err := os.MkdirTemp(r.root, ".delta-staging-")
	if err != nil {
		return DeltaManifest{}, false, err
	}
	defer os.RemoveAll(staging)
	extracted, checksums, err := extractAndVerifyZip(reader, staging, maximumBytes, []string{snapshotManifestFile, checksumsFile, deltaDatabaseFile})
	if err != nil {
		return DeltaManifest{}, false, err
	}
	manifest, rows, err := verifyDelta(extracted, deltaID)
	if err != nil {
		return DeltaManifest{}, false, err
	}
	destination := filepath.Join(r.deltas, deltaID)
	if err := installImmutableDirectory(extracted, destination, checksums); err != nil {
		return DeltaManifest{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	active, err := r.store.Active(ctx)
	if err != nil {
		return DeltaManifest{}, false, err
	}
	databaseChecksum := checksums[deltaDatabaseFile]
	if previousChecksum, found, err := r.store.AppliedDelta(ctx, deltaID); err != nil {
		return DeltaManifest{}, false, err
	} else if found {
		if previousChecksum != databaseChecksum {
			return DeltaManifest{}, false, ErrVersionConflict
		}
		return manifest, false, nil
	}
	if active.ModelVersion == "" || active.ActiveWatermark != manifest.FromWatermark {
		return DeltaManifest{}, false, fmt.Errorf("%w: active=%q delta_from=%q", ErrWatermarkConflict, active.ActiveWatermark, manifest.FromWatermark)
	}
	applied, err := r.store.ApplyDelta(ctx, deltaID, manifest.ToWatermark, databaseChecksum, rows)
	if err != nil {
		if strings.Contains(err.Error(), "different checksum") {
			return DeltaManifest{}, false, ErrVersionConflict
		}
		return DeltaManifest{}, false, err
	}
	return manifest, applied, nil
}

func extractAndVerifyZip(reader io.Reader, staging string, maximumBytes int64, required []string) (string, map[string]string, error) {
	if maximumBytes <= 0 {
		return "", nil, fmt.Errorf("%w: invalid size limit", ErrInvalidBundle)
	}
	archivePath := filepath.Join(staging, "bundle.zip")
	archive, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, err
	}
	written, copyErr := io.Copy(archive, io.LimitReader(reader, maximumBytes+1))
	closeErr := archive.Close()
	if copyErr != nil {
		return "", nil, copyErr
	}
	if closeErr != nil {
		return "", nil, closeErr
	}
	if written > maximumBytes {
		return "", nil, fmt.Errorf("%w: bundle exceeds size limit", ErrInvalidBundle)
	}
	zipReader, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", nil, fmt.Errorf("%w: open zip: %v", ErrInvalidBundle, err)
	}
	defer zipReader.Close()
	wanted := make(map[string]bool, len(required))
	for _, name := range required {
		wanted[name] = true
	}
	extracted := filepath.Join(staging, "bundle")
	if err := os.Mkdir(extracted, 0o750); err != nil {
		return "", nil, err
	}
	var unpacked int64
	seen := make(map[string]bool, len(required))
	for _, file := range zipReader.File {
		name := filepath.ToSlash(file.Name)
		if !wanted[name] || seen[name] || file.FileInfo().IsDir() || file.Mode()&os.ModeSymlink != 0 {
			return "", nil, fmt.Errorf("%w: unexpected zip entry %q", ErrInvalidBundle, name)
		}
		unpacked += int64(file.UncompressedSize64)
		if unpacked > maximumBytes {
			return "", nil, fmt.Errorf("%w: uncompressed bundle exceeds size limit", ErrInvalidBundle)
		}
		source, err := file.Open()
		if err != nil {
			return "", nil, err
		}
		destination, err := os.OpenFile(filepath.Join(extracted, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			source.Close()
			return "", nil, err
		}
		_, copyErr := io.Copy(destination, source)
		sourceErr, destinationErr := source.Close(), destination.Close()
		if copyErr != nil {
			return "", nil, copyErr
		}
		if sourceErr != nil {
			return "", nil, sourceErr
		}
		if destinationErr != nil {
			return "", nil, destinationErr
		}
		seen[name] = true
	}
	if len(seen) != len(required) {
		return "", nil, fmt.Errorf("%w: required files are missing", ErrInvalidBundle)
	}
	checksums, err := readChecksums(filepath.Join(extracted, checksumsFile))
	if err != nil {
		return "", nil, err
	}
	for _, name := range required {
		if name == checksumsFile {
			continue
		}
		expected, ok := checksums[name]
		if !ok {
			return "", nil, fmt.Errorf("%w: checksum missing for %s", ErrInvalidBundle, name)
		}
		actual, err := fileChecksum(filepath.Join(extracted, name))
		if err != nil {
			return "", nil, err
		}
		if actual != expected {
			return "", nil, fmt.Errorf("%w: checksum mismatch for %s", ErrInvalidBundle, name)
		}
	}
	return extracted, checksums, nil
}

func readChecksums(path string) (map[string]string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var checksums map[string]string
	if err := json.Unmarshal(payload, &checksums); err != nil || len(checksums) == 0 {
		return nil, fmt.Errorf("%w: invalid checksums.json", ErrInvalidBundle)
	}
	for name, checksum := range checksums {
		if filepath.Base(name) != name || len(checksum) != 64 {
			return nil, fmt.Errorf("%w: invalid checksum entry", ErrInvalidBundle)
		}
	}
	return checksums, nil
}

func fileChecksum(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifySnapshot(directory, version string) (SnapshotManifest, error) {
	manifestPath := filepath.Join(directory, snapshotManifestFile)
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		return SnapshotManifest{}, err
	}
	var manifest SnapshotManifest
	if err := json.Unmarshal(payload, &manifest); err != nil || manifest.SchemaVersion != 1 || manifest.Kind != "history_snapshot" || manifest.ModelVersion != version || manifest.SourceWatermark == "" || manifest.CreatedAt.IsZero() {
		return SnapshotManifest{}, fmt.Errorf("%w: invalid snapshot manifest", ErrInvalidBundle)
	}
	if err := verifySQLite(filepath.Join(directory, snapshotDatabaseFile), []string{"repo_history_series", "repository_metadata", "history_active", "applied_deltas"}); err != nil {
		return SnapshotManifest{}, err
	}
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(directory, snapshotDatabaseFile))+"?mode=ro")
	if err != nil {
		return SnapshotManifest{}, err
	}
	defer database.Close()
	var repositories, eventDays, watchEvents int64
	if err := database.QueryRow(`SELECT COUNT(*), COALESCE(SUM(point_count), 0), COALESCE(SUM(event_total), 0) FROM repo_history_series`).Scan(&repositories, &eventDays, &watchEvents); err != nil {
		return SnapshotManifest{}, err
	}
	if repositories != manifest.Repositories || eventDays != manifest.EventDays || watchEvents != manifest.WatchEvents {
		return SnapshotManifest{}, fmt.Errorf("%w: snapshot statistics do not match manifest", ErrInvalidBundle)
	}
	var activeVersion, activeWatermark string
	if err := database.QueryRow(`SELECT model_version, active_watermark FROM history_active WHERE id = 1`).Scan(&activeVersion, &activeWatermark); err != nil || activeVersion != manifest.ModelVersion || activeWatermark != manifest.SourceWatermark {
		return SnapshotManifest{}, fmt.Errorf("%w: snapshot active state does not match manifest", ErrInvalidBundle)
	}
	return manifest, nil
}

func verifyDelta(directory, deltaID string) (DeltaManifest, []DeltaRow, error) {
	payload, err := os.ReadFile(filepath.Join(directory, snapshotManifestFile))
	if err != nil {
		return DeltaManifest{}, nil, err
	}
	var manifest DeltaManifest
	if err := json.Unmarshal(payload, &manifest); err != nil || manifest.SchemaVersion != 1 || manifest.Kind != "history_delta" || manifest.DeltaID != deltaID || manifest.FromWatermark == "" || manifest.ToWatermark == "" || manifest.CreatedAt.IsZero() || !checksumPattern.MatchString(manifest.SourceChecksum) {
		return DeltaManifest{}, nil, fmt.Errorf("%w: invalid delta manifest", ErrInvalidBundle)
	}
	fromDate, fromErr := time.Parse("2006-01-02", manifest.FromWatermark)
	toDate, toErr := time.Parse("2006-01-02", manifest.ToWatermark)
	if fromErr != nil || toErr != nil || !toDate.Equal(fromDate.AddDate(0, 0, 1)) {
		return DeltaManifest{}, nil, fmt.Errorf("%w: delta watermarks must be adjacent UTC dates", ErrInvalidBundle)
	}
	path := filepath.Join(directory, deltaDatabaseFile)
	if err := verifySQLite(path, []string{"repo_star_daily_delta"}); err != nil {
		return DeltaManifest{}, nil, err
	}
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return DeltaManifest{}, nil, err
	}
	defer database.Close()
	rows, err := database.Query(`SELECT repo_id, event_day, event_count FROM repo_star_daily_delta ORDER BY repo_id, event_day`)
	if err != nil {
		return DeltaManifest{}, nil, err
	}
	defer rows.Close()
	capacity := int64(1_000_000)
	if manifest.Rows < capacity {
		capacity = manifest.Rows
	}
	if capacity < 0 {
		return DeltaManifest{}, nil, fmt.Errorf("%w: invalid delta row count", ErrInvalidBundle)
	}
	values := make([]DeltaRow, 0, int(capacity))
	for rows.Next() {
		var value DeltaRow
		var count int64
		if err := rows.Scan(&value.RepoID, &value.EventDay, &count); err != nil || value.RepoID <= 0 || value.EventDay < 0 || count <= 0 {
			return DeltaManifest{}, nil, fmt.Errorf("%w: invalid delta row", ErrInvalidBundle)
		}
		value.EventCount = uint64(count)
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return DeltaManifest{}, nil, err
	}
	if int64(len(values)) != manifest.Rows {
		return DeltaManifest{}, nil, fmt.Errorf("%w: delta row count mismatch", ErrInvalidBundle)
	}
	return manifest, values, nil
}

func verifySQLite(path string, requiredTables []string) error {
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return err
	}
	defer database.Close()
	var result string
	if err := database.QueryRow(`PRAGMA quick_check`).Scan(&result); err != nil || result != "ok" {
		return fmt.Errorf("%w: SQLite quick_check failed", ErrInvalidBundle)
	}
	for _, table := range requiredTables {
		var found string
		if err := database.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&found); err != nil {
			return fmt.Errorf("%w: required table %s is missing", ErrInvalidBundle, table)
		}
	}
	return nil
}

func installImmutableDirectory(source, destination string, checksums map[string]string) error {
	if _, err := os.Stat(destination); err == nil {
		stored, readErr := readChecksums(filepath.Join(destination, checksumsFile))
		if readErr != nil {
			return readErr
		}
		if !equalChecksums(stored, checksums) {
			return ErrVersionConflict
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(source, destination)
}

func equalChecksums(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	keys := make([]string, 0, len(left))
	for key := range left {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if left[key] != right[key] {
			return false
		}
	}
	return true
}
