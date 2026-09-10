// Package serving 管理 History Serving SQLite 的读取与增量写入。
package serving

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/model"
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
	RepoID        int64
	FullName      string
	Visibility    string
	CurrentStars  int
	CheckedAt     time.Time
	Description   string
	Language      string
	Topics        []string
	CreatedAt     time.Time
	AvatarURL     string
	AvatarDataURI string
}

// ActiveState 描述当前对外服务的数据版本。
type ActiveState struct {
	ModelVersion    string    `json:"model_version"`
	ActiveWatermark string    `json:"active_watermark"`
	GeneratedAt     time.Time `json:"generated_at"`
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
			checked_at TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			language TEXT NOT NULL DEFAULT '',
			topics_json TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL DEFAULT '',
			avatar_url TEXT NOT NULL DEFAULT '',
			avatar_data_uri TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS github_star_history_cache (
			owner TEXT NOT NULL,
			repo TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			response_etag TEXT NOT NULL DEFAULT '',
			fetched_at TEXT NOT NULL,
			full_history_validated_at TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(owner, repo)
		)`,
		`CREATE INDEX IF NOT EXISTS repository_metadata_full_name_index
			ON repository_metadata(full_name COLLATE NOCASE)`,
		`CREATE TABLE IF NOT EXISTS history_active (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			model_version TEXT NOT NULL,
			active_watermark TEXT NOT NULL,
			generated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS history_statistics (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			repositories INTEGER NOT NULL,
			event_days INTEGER NOT NULL,
			watch_events INTEGER NOT NULL,
			metadata_entries INTEGER NOT NULL
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
	if err := s.ensureRepositoryMetadataColumns(ctx); err != nil {
		return err
	}
	return nil
}

// GitHubStarHistoryCache 读取官方周数据缓存。缓存按规范化 owner/repo 键控，
// 与 Serving 的 repo_id 无关，避免官方历史接口被旧 GH Archive 数据库结构限制。
func (s *Store) GitHubStarHistoryCache(ctx context.Context, owner, repo string) (model.GitHubStarHistoryCache, bool, error) {
	var payload, etag, fetchedAt, validatedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT payload_json, response_etag, fetched_at, full_history_validated_at
		FROM github_star_history_cache WHERE owner = ? AND repo = ?`,
		normalizeCachePart(owner), normalizeCachePart(repo)).Scan(&payload, &etag, &fetchedAt, &validatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.GitHubStarHistoryCache{}, false, nil
	}
	if err != nil {
		return model.GitHubStarHistoryCache{}, false, err
	}
	var result model.GitHubStarHistoryCache
	if err := json.Unmarshal([]byte(payload), &result.Weeks); err != nil {
		return model.GitHubStarHistoryCache{}, false, fmt.Errorf("parse github star history cache: %w", err)
	}
	result.ResponseETag = etag
	result.FetchedAt, err = time.Parse(time.RFC3339Nano, fetchedAt)
	if err != nil {
		return model.GitHubStarHistoryCache{}, false, fmt.Errorf("parse github star history fetched_at: %w", err)
	}
	if validatedAt != "" {
		result.FullHistoryValidatedAt, err = time.Parse(time.RFC3339Nano, validatedAt)
		if err != nil {
			return model.GitHubStarHistoryCache{}, false, fmt.Errorf("parse github star history full_history_validated_at: %w", err)
		}
	}
	return result, true, nil
}

// SaveGitHubStarHistoryCache 原子保存官方历史原始周数据。raw payload 可重新构建，
// 所以只追加新表，不触碰已经发布的 repo_history_series schema。
func (s *Store) SaveGitHubStarHistoryCache(ctx context.Context, owner, repo string, value model.GitHubStarHistoryCache) error {
	payload, err := json.Marshal(value.Weeks)
	if err != nil {
		return fmt.Errorf("encode github star history cache: %w", err)
	}
	validatedAt := ""
	if !value.FullHistoryValidatedAt.IsZero() {
		validatedAt = value.FullHistoryValidatedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO github_star_history_cache (
			owner, repo, payload_json, response_etag, fetched_at, full_history_validated_at
		) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(owner, repo) DO UPDATE SET
			payload_json=excluded.payload_json,
			response_etag=excluded.response_etag,
			fetched_at=excluded.fetched_at,
			full_history_validated_at=excluded.full_history_validated_at`,
		normalizeCachePart(owner), normalizeCachePart(repo), string(payload), value.ResponseETag,
		value.FetchedAt.UTC().Format(time.RFC3339Nano), validatedAt)
	return err
}

// TouchGitHubStarHistoryCache 处理官方接口 304：数据不变，只推进新鲜时间。
func (s *Store) TouchGitHubStarHistoryCache(ctx context.Context, owner, repo string, fetchedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE github_star_history_cache SET fetched_at = ? WHERE owner = ? AND repo = ?`,
		fetchedAt.UTC().Format(time.RFC3339Nano), normalizeCachePart(owner), normalizeCachePart(repo))
	return err
}

func normalizeCachePart(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

// ensureRepositoryMetadataColumns 以追加列方式升级已有 Serving 库。
// 该服务已经有本地与线上数据库，不能依赖用户删除旧库；列名是代码常量，避免动态 SQL 引入额外输入面。
func (s *Store) ensureRepositoryMetadataColumns(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(repository_metadata)`)
	if err != nil {
		return fmt.Errorf("inspect repository metadata schema: %w", err)
	}
	defer rows.Close()
	columns := make(map[string]struct{})
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("read repository metadata schema: %w", err)
		}
		columns[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate repository metadata schema: %w", err)
	}
	for _, column := range []string{
		`description TEXT NOT NULL DEFAULT ''`,
		`language TEXT NOT NULL DEFAULT ''`,
		`topics_json TEXT NOT NULL DEFAULT '[]'`,
		`created_at TEXT NOT NULL DEFAULT ''`,
		`avatar_url TEXT NOT NULL DEFAULT ''`,
		`avatar_data_uri TEXT NOT NULL DEFAULT ''`,
	} {
		name := strings.Fields(column)[0]
		if _, exists := columns[name]; exists {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE repository_metadata ADD COLUMN `+column); err != nil {
			return fmt.Errorf("add repository metadata column %s: %w", name, err)
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
	result, err := readMetadata(s.db.QueryRowContext(ctx, repositoryMetadataSelect+` WHERE repo_id = ?`, repoID))
	if errors.Is(err, sql.ErrNoRows) {
		return RepositoryMetadata{}, false, nil
	}
	if err != nil {
		return RepositoryMetadata{}, false, err
	}
	return result, true, nil
}

// MetadataByFullName 按 GitHub canonical full_name 查找 metadata，供无 repo_id 的公开图片入口使用。
// 查询大小写不敏感，与 GitHub owner/repository URL 的语义保持一致。
func (s *Store) MetadataByFullName(ctx context.Context, fullName string) (RepositoryMetadata, bool, error) {
	result, err := readMetadata(s.db.QueryRowContext(ctx, repositoryMetadataSelect+` WHERE full_name = ? COLLATE NOCASE ORDER BY checked_at DESC LIMIT 1`, strings.TrimSpace(fullName)))
	if errors.Is(err, sql.ErrNoRows) {
		return RepositoryMetadata{}, false, nil
	}
	if err != nil {
		return RepositoryMetadata{}, false, err
	}
	return result, true, nil
}

const repositoryMetadataSelect = `
	SELECT repo_id, full_name, visibility, current_stars, checked_at,
	       description, language, topics_json, created_at, avatar_url, avatar_data_uri
	FROM repository_metadata`

type metadataRow interface {
	Scan(dest ...any) error
}

func readMetadata(row metadataRow) (RepositoryMetadata, error) {
	var result RepositoryMetadata
	var checkedAt, topicsJSON, createdAt string
	if err := row.Scan(
		&result.RepoID, &result.FullName, &result.Visibility, &result.CurrentStars, &checkedAt,
		&result.Description, &result.Language, &topicsJSON, &createdAt, &result.AvatarURL, &result.AvatarDataURI,
	); err != nil {
		return RepositoryMetadata{}, err
	}
	var err error
	result.CheckedAt, err = time.Parse(time.RFC3339Nano, checkedAt)
	if err != nil {
		return RepositoryMetadata{}, fmt.Errorf("parse metadata checked_at: %w", err)
	}
	if topicsJSON == "" {
		topicsJSON = "[]"
	}
	if err := json.Unmarshal([]byte(topicsJSON), &result.Topics); err != nil {
		return RepositoryMetadata{}, fmt.Errorf("parse metadata topics: %w", err)
	}
	if createdAt != "" {
		result.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return RepositoryMetadata{}, fmt.Errorf("parse metadata created_at: %w", err)
		}
	}
	return result, nil
}

// SaveMetadata 原子更新 GitHub 元数据缓存，并只在首次插入时推进统计计数。
func (s *Store) SaveMetadata(ctx context.Context, value RepositoryMetadata) error {
	topics := value.Topics
	if topics == nil {
		topics = []string{}
	}
	topicsJSON, err := json.Marshal(topics)
	if err != nil {
		return fmt.Errorf("encode metadata topics: %w", err)
	}
	createdAt := ""
	if !value.CreatedAt.IsZero() {
		createdAt = value.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO history_statistics (id, repositories, event_days, watch_events, metadata_entries)
		VALUES (1, 0, 0, 0, 0) ON CONFLICT(id) DO NOTHING`); err != nil {
		return err
	}
	var existing int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM repository_metadata WHERE repo_id = ?`, value.RepoID).Scan(&existing)
	isNew := errors.Is(err, sql.ErrNoRows)
	if err != nil && !isNew {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO repository_metadata (
			repo_id, full_name, visibility, current_stars, checked_at,
			description, language, topics_json, created_at, avatar_url, avatar_data_uri
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_id) DO UPDATE SET
			full_name=excluded.full_name,
			visibility=excluded.visibility,
			current_stars=excluded.current_stars,
			checked_at=excluded.checked_at,
			description=excluded.description,
			language=excluded.language,
			topics_json=excluded.topics_json,
			created_at=excluded.created_at,
			avatar_url=excluded.avatar_url,
			avatar_data_uri=excluded.avatar_data_uri`,
		value.RepoID, value.FullName, value.Visibility, value.CurrentStars,
		value.CheckedAt.UTC().Format(time.RFC3339Nano), value.Description, value.Language,
		string(topicsJSON), createdAt, value.AvatarURL, value.AvatarDataURI,
	); err != nil {
		return err
	}
	if isNew {
		if _, err := tx.ExecContext(ctx, `UPDATE history_statistics SET metadata_entries = metadata_entries + 1 WHERE id = 1`); err != nil {
			return err
		}
	}
	return tx.Commit()
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

// EnsureStatistics 为旧 Snapshot 首次创建常量时间统计行；已经应用过 Delta 的计数不得覆盖。
func (s *Store) EnsureStatistics(ctx context.Context, value Stats) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO history_statistics (id, repositories, event_days, watch_events, metadata_entries)
		VALUES (1, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		value.Repositories, value.EventDays, value.WatchEvents, value.MetadataEntries,
	)
	return err
}

// OperationalStats 返回控制台展示所需的常量时间统计，禁止在请求路径扫描全量序列表。
func (s *Store) OperationalStats(ctx context.Context) (Stats, error) {
	var result Stats
	err := s.db.QueryRowContext(ctx, `
		SELECT repositories, event_days, watch_events, metadata_entries
		FROM history_statistics WHERE id = 1`).Scan(
		&result.Repositories, &result.EventDays, &result.WatchEvents, &result.MetadataEntries,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
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
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO history_statistics (id, repositories, event_days, watch_events, metadata_entries)
		VALUES (1, 0, 0, 0, 0) ON CONFLICT(id) DO NOTHING`); err != nil {
		return false, err
	}

	grouped := make(map[int64][]series.DayCount)
	for _, row := range rows {
		if row.RepoID <= 0 || row.EventDay < 0 || row.EventCount == 0 {
			return false, fmt.Errorf("invalid delta row")
		}
		grouped[row.RepoID] = append(grouped[row.RepoID], series.DayCount{Day: row.EventDay, Count: row.EventCount})
	}
	var repositoryDelta, pointDelta, eventDelta int64
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
		isNewRepository := errors.Is(err, sql.ErrNoRows)
		if isNewRepository {
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
		if isNewRepository {
			repositoryDelta++
		}
		pointDelta += int64(len(merged) - len(existing))
		eventDelta += int64(total) - eventTotal
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
	if _, err := tx.ExecContext(ctx, `
		UPDATE history_statistics
		SET repositories = repositories + ?, event_days = event_days + ?, watch_events = watch_events + ?
		WHERE id = 1`, repositoryDelta, pointDelta, eventDelta); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
