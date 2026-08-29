// Package serving 管理 History Serving SQLite 的读取与增量写入。
package serving

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/series"

	_ "modernc.org/sqlite"
)

var ErrSeriesNotFound = errors.New("history series not found")

// RepositorySeries 是一条完整的仓库 WatchEvent 压缩序列。
type RepositorySeries struct {
	RepoID           int64
	CoverageStartDay int
	CoverageEndDay   int
	EventTotal       uint64
	PointCount       int
	Encoding         string
	Series           []byte
	SourceWatermark  string
	SeriesChecksum   string
}

// RepositoryMetadata 缓存 GitHub 当前公开元数据，避免每次历史查询都访问 GitHub。
type RepositoryMetadata struct {
	RepoID       int64
	FullName     string
	Visibility   string
	CurrentStars int
	CheckedAt    time.Time
}

// ActiveState 描述当前对外服务的数据版本。
type ActiveState struct {
	ModelVersion    string
	ActiveWatermark string
	GeneratedAt     time.Time
}

// Stats 是控制台可消费的有界运行统计。
type Stats struct {
	Repositories    int64       `json:"repositories"`
	EventDays       int64       `json:"event_days"`
	WatchEvents     int64       `json:"watch_events"`
	MetadataEntries int64       `json:"metadata_entries"`
	DatabaseBytes   int64       `json:"database_bytes"`
	Active          ActiveState `json:"active"`
}

// DeltaRow 是本地 Builder 生成的 repo/day 增量。
type DeltaRow struct {
	RepoID     int64
	EventDay   int
	EventCount uint64
}

// Store 封装单个 Serving SQLite。快照切换由 Registry 在进程外层串行完成。
type Store struct {
	db   *sql.DB
	path string
}

// Open 打开数据库并补齐服务自身需要的 schema。
func Open(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = ":memory:"
	}
	if path != ":memory:" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		path = absolute
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, err
		}
	}
	dsn := path
	if path != ":memory:" {
		dsn = "file:" + filepath.ToSlash(path)
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	store := &Store{db: database, path: path}
	if err := store.initialize(context.Background()); err != nil {
		database.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) initialize(ctx context.Context) error {
	statements := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		`CREATE TABLE IF NOT EXISTS repo_history_series (
			repo_id INTEGER PRIMARY KEY,
			coverage_start_day INTEGER NOT NULL,
			coverage_end_day INTEGER NOT NULL,
			event_total INTEGER NOT NULL,
			point_count INTEGER NOT NULL,
			encoding TEXT NOT NULL,
			series BLOB NOT NULL,
			source_watermark TEXT NOT NULL,
			series_checksum TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS repository_metadata (
			repo_id INTEGER PRIMARY KEY,
			full_name TEXT NOT NULL,
			visibility TEXT NOT NULL,
			current_stars INTEGER NOT NULL,
			checked_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS history_active (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			model_version TEXT NOT NULL,
			active_watermark TEXT NOT NULL,
			generated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS applied_deltas (
			delta_id TEXT PRIMARY KEY,
			watermark TEXT NOT NULL,
			checksum TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize serving schema: %w", err)
		}
	}
	return nil
}

// Close 释放 SQLite 连接。
func (s *Store) Close() error { return s.db.Close() }

// Path 返回当前数据库路径，仅用于 Registry 的受控快照切换。
func (s *Store) Path() string { return s.path }

// Series 读取单仓压缩序列。
func (s *Store) Series(ctx context.Context, repoID int64) (RepositorySeries, error) {
	var result RepositorySeries
	var eventTotal int64
	err := s.db.QueryRowContext(ctx, `
		SELECT repo_id, coverage_start_day, coverage_end_day, event_total, point_count,
		       encoding, series, source_watermark, series_checksum
		FROM repo_history_series WHERE repo_id = ?`, repoID).Scan(
		&result.RepoID, &result.CoverageStartDay, &result.CoverageEndDay, &eventTotal,
		&result.PointCount, &result.Encoding, &result.Series, &result.SourceWatermark,
		&result.SeriesChecksum,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return RepositorySeries{}, ErrSeriesNotFound
	}
	if err != nil {
		return RepositorySeries{}, err
	}
	if eventTotal < 0 {
		return RepositorySeries{}, fmt.Errorf("negative event total for repo %d", repoID)
	}
	result.EventTotal = uint64(eventTotal)
	return result, nil
}

// UpsertSeries 写入 Builder 或增量合并后的完整序列。
func (s *Store) UpsertSeries(ctx context.Context, value RepositorySeries) error {
	if value.EventTotal > uint64(^uint64(0)>>1) {
		return fmt.Errorf("event total overflow")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO repo_history_series (
			repo_id, coverage_start_day, coverage_end_day, event_total, point_count,
			encoding, series, source_watermark, series_checksum
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_id) DO UPDATE SET
			coverage_start_day=excluded.coverage_start_day,
			coverage_end_day=excluded.coverage_end_day,
			event_total=excluded.event_total,
			point_count=excluded.point_count,
			encoding=excluded.encoding,
			series=excluded.series,
			source_watermark=excluded.source_watermark,
			series_checksum=excluded.series_checksum`,
		value.RepoID, value.CoverageStartDay, value.CoverageEndDay, int64(value.EventTotal),
		value.PointCount, value.Encoding, value.Series, value.SourceWatermark, value.SeriesChecksum,
	)
	return err
}

// Metadata 读取 GitHub 元数据缓存。
func (s *Store) Metadata(ctx context.Context, repoID int64) (RepositoryMetadata, bool, error) {
	var result RepositoryMetadata
	var checkedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT repo_id, full_name, visibility, current_stars, checked_at
		FROM repository_metadata WHERE repo_id = ?`, repoID).Scan(
		&result.RepoID, &result.FullName, &result.Visibility, &result.CurrentStars, &checkedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return RepositoryMetadata{}, false, nil
	}
	if err != nil {
		return RepositoryMetadata{}, false, err
	}
	result.CheckedAt, err = time.Parse(time.RFC3339Nano, checkedAt)
	if err != nil {
		return RepositoryMetadata{}, false, fmt.Errorf("parse metadata checked_at: %w", err)
	}
	return result, true, nil
}

// SaveMetadata 原子更新 GitHub 元数据缓存。
func (s *Store) SaveMetadata(ctx context.Context, value RepositoryMetadata) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO repository_metadata (repo_id, full_name, visibility, current_stars, checked_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(repo_id) DO UPDATE SET
			full_name=excluded.full_name,
			visibility=excluded.visibility,
			current_stars=excluded.current_stars,
			checked_at=excluded.checked_at`,
		value.RepoID, value.FullName, value.Visibility, value.CurrentStars,
		value.CheckedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

// Active 返回当前数据版本；空库返回零值而不是错误，便于健康检查启动。
func (s *Store) Active(ctx context.Context) (ActiveState, error) {
	var result ActiveState
	var generatedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT model_version, active_watermark, generated_at FROM history_active WHERE id = 1`).Scan(
		&result.ModelVersion, &result.ActiveWatermark, &generatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ActiveState{}, nil
	}
	if err != nil {
		return ActiveState{}, err
	}
	result.GeneratedAt, err = time.Parse(time.RFC3339Nano, generatedAt)
	if err != nil {
		return ActiveState{}, err
	}
	return result, nil
}

// SetActive 由快照发布或增量应用在同一 SQLite 上更新水位。
func (s *Store) SetActive(ctx context.Context, state ActiveState) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO history_active (id, model_version, active_watermark, generated_at)
		VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			model_version=excluded.model_version,
			active_watermark=excluded.active_watermark,
			generated_at=excluded.generated_at`,
		state.ModelVersion, state.ActiveWatermark, state.GeneratedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

// AppliedDelta 返回已应用增量的内容校验和，用于在水位检查前识别安全重放。
func (s *Store) AppliedDelta(ctx context.Context, deltaID string) (string, bool, error) {
	var checksum string
	err := s.db.QueryRowContext(ctx, `SELECT checksum FROM applied_deltas WHERE delta_id = ?`, deltaID).Scan(&checksum)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return checksum, true, nil
}

// OperationalStats 返回控制台展示所需的小结果集，不扫描 BLOB 内容。
func (s *Store) OperationalStats(ctx context.Context) (Stats, error) {
	var result Stats
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(point_count), 0), COALESCE(SUM(event_total), 0)
		FROM repo_history_series`).Scan(&result.Repositories, &result.EventDays, &result.WatchEvents)
	if err != nil {
		return Stats{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM repository_metadata`).Scan(&result.MetadataEntries); err != nil {
		return Stats{}, err
	}
	result.Active, err = s.Active(ctx)
	if err != nil {
		return Stats{}, err
	}
	if s.path != ":memory:" {
		info, statErr := os.Stat(s.path)
		if statErr != nil {
			return Stats{}, statErr
		}
		result.DatabaseBytes = info.Size()
	}
	return result, nil
}

// ApplyDelta 在单个事务内合并增量、登记幂等键并推进 active watermark。
// delta_id 相同且 checksum 相同视为重放成功；内容不同则拒绝覆盖。
func (s *Store) ApplyDelta(ctx context.Context, deltaID, watermark, checksum string, rows []DeltaRow) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var existingChecksum string
	err = tx.QueryRowContext(ctx, `SELECT checksum FROM applied_deltas WHERE delta_id = ?`, deltaID).Scan(&existingChecksum)
	if err == nil {
		if existingChecksum != checksum {
			return false, fmt.Errorf("delta id already exists with different checksum")
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	grouped := make(map[int64][]series.DayCount)
	for _, row := range rows {
		if row.RepoID <= 0 || row.EventDay < 0 || row.EventCount == 0 {
			return false, fmt.Errorf("invalid delta row")
		}
		grouped[row.RepoID] = append(grouped[row.RepoID], series.DayCount{Day: row.EventDay, Count: row.EventCount})
	}
	for repoID, additions := range grouped {
		var current RepositorySeries
		var eventTotal int64
		err := tx.QueryRowContext(ctx, `
			SELECT repo_id, coverage_start_day, coverage_end_day, event_total, point_count,
			       encoding, series, source_watermark, series_checksum
			FROM repo_history_series WHERE repo_id = ?`, repoID).Scan(
			&current.RepoID, &current.CoverageStartDay, &current.CoverageEndDay, &eventTotal,
			&current.PointCount, &current.Encoding, &current.Series, &current.SourceWatermark,
			&current.SeriesChecksum,
		)
		var existing []series.DayCount
		if errors.Is(err, sql.ErrNoRows) {
			current.RepoID = repoID
			current.Encoding = series.Encoding
		} else if err != nil {
			return false, err
		} else {
			existing, err = series.Decode(current.Encoding, current.Series, current.SeriesChecksum, current.PointCount)
			if err != nil {
				return false, err
			}
		}
		merged, err := series.Merge(existing, additions)
		if err != nil {
			return false, err
		}
		payload, seriesChecksum, err := series.Encode(merged)
		if err != nil {
			return false, err
		}
		var total uint64
		for _, point := range merged {
			total += point.Count
		}
		if total > uint64(^uint64(0)>>1) {
			return false, fmt.Errorf("event total overflow")
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO repo_history_series (
				repo_id, coverage_start_day, coverage_end_day, event_total, point_count,
				encoding, series, source_watermark, series_checksum
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(repo_id) DO UPDATE SET
				coverage_start_day=excluded.coverage_start_day,
				coverage_end_day=excluded.coverage_end_day,
				event_total=excluded.event_total,
				point_count=excluded.point_count,
				encoding=excluded.encoding,
				series=excluded.series,
				source_watermark=excluded.source_watermark,
				series_checksum=excluded.series_checksum`,
			repoID, merged[0].Day, merged[len(merged)-1].Day, int64(total), len(merged),
			series.Encoding, payload, watermark, seriesChecksum,
		)
		if err != nil {
			return false, err
		}
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO applied_deltas (delta_id, watermark, checksum, applied_at) VALUES (?, ?, ?, ?)`,
		deltaID, watermark, checksum, now.Format(time.RFC3339Nano)); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE history_active SET active_watermark = ?, generated_at = ? WHERE id = 1`,
		watermark, now.Format(time.RFC3339Nano)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
