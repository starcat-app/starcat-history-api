"""Snapshot 与 Delta SQLite schema 的单一来源。"""

SNAPSHOT_SCHEMA = """
CREATE TABLE repo_history_series (
    repo_id INTEGER PRIMARY KEY,
    coverage_start_day INTEGER NOT NULL,
    coverage_end_day INTEGER NOT NULL,
    event_total INTEGER NOT NULL,
    point_count INTEGER NOT NULL,
    encoding TEXT NOT NULL,
    series BLOB NOT NULL,
    source_watermark TEXT NOT NULL,
    series_checksum TEXT NOT NULL
);
CREATE TABLE repository_metadata (
    repo_id INTEGER PRIMARY KEY,
    full_name TEXT NOT NULL,
    visibility TEXT NOT NULL,
    current_stars INTEGER NOT NULL,
    checked_at TEXT NOT NULL
);
CREATE TABLE history_active (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    model_version TEXT NOT NULL,
    active_watermark TEXT NOT NULL,
    generated_at TEXT NOT NULL
);
CREATE TABLE applied_deltas (
    delta_id TEXT PRIMARY KEY,
    watermark TEXT NOT NULL,
    checksum TEXT NOT NULL,
    applied_at TEXT NOT NULL
);
"""

DELTA_SCHEMA = """
CREATE TABLE repo_star_daily_delta (
    repo_id INTEGER NOT NULL,
    event_day INTEGER NOT NULL,
    event_count INTEGER NOT NULL,
    PRIMARY KEY (repo_id, event_day)
);
"""

