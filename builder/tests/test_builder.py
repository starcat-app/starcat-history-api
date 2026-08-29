"""Builder 的最小真实 Parquet -> SQLite -> ZIP 验证。"""

from __future__ import annotations

import json
import sqlite3
import zipfile
from pathlib import Path

import duckdb

from starcat_history_builder.build import DuckDBOptions, build_delta, build_silver, build_snapshot


def _parquet(path: Path) -> None:
    connection = duckdb.connect()
    connection.execute(
        """
        COPY (
            SELECT * FROM (VALUES
                ('a', 'u1', 7::BIGINT, TIMESTAMPTZ '2026-08-24 01:00:00+00'),
                ('b', 'u2', 7::BIGINT, TIMESTAMPTZ '2026-08-24 02:00:00+00'),
                ('c', 'u3', 7::BIGINT, TIMESTAMPTZ '2026-08-25 01:00:00+00'),
                ('d', 'u4', 8::BIGINT, TIMESTAMPTZ '2026-08-25 01:00:00+00')
            ) AS events(source_record_id, actor_id, repo_id, created_at)
        ) TO ? (FORMAT PARQUET)
        """,
        [str(path)],
    )
    connection.close()


def _canonical_parquet(path: Path) -> None:
    connection = duckdb.connect()
    connection.execute(
        """
        COPY (
            SELECT * FROM (VALUES
                (7::BIGINT, 'star_event', TIMESTAMPTZ '2026-08-25 01:00:00+00'),
                (7::BIGINT, 'star_event', TIMESTAMPTZ '2026-08-25 02:00:00+00'),
                (7::BIGINT, 'push_event', TIMESTAMPTZ '2026-08-25 03:00:00+00')
            ) AS events(repo_id, relation_type, occurred_at)
        ) TO ? (FORMAT PARQUET)
        """,
        [str(path)],
    )
    connection.close()


def test_build_snapshot_and_delta(tmp_path: Path) -> None:
    source = tmp_path / "watch.parquet"
    _parquet(source)
    options = DuckDBOptions(temp_directory=tmp_path / "spill", memory_limit="1GB", threads=1)
    snapshot_zip = build_snapshot([str(source)], tmp_path / "out", "fixture-v1", "2026-08-25", options)
    assert snapshot_zip.is_file()
    snapshot_dir = snapshot_zip.parent
    manifest = json.loads((snapshot_dir / "manifest.json").read_text())
    assert manifest["repositories"] == 2
    assert manifest["event_days"] == 3
    assert manifest["watch_events"] == 4
    with sqlite3.connect(snapshot_dir / "history.sqlite") as database:
        assert database.execute("SELECT point_count, event_total FROM repo_history_series WHERE repo_id=7").fetchone() == (2, 3)
        assert database.execute("PRAGMA quick_check").fetchone()[0] == "ok"
    with zipfile.ZipFile(snapshot_zip) as archive:
        assert sorted(archive.namelist()) == ["checksums.json", "history.sqlite", "manifest.json"]

    delta_zip = build_delta([str(source)], tmp_path / "delta-out", "delta-1", "2026-08-24", "2026-08-25", options)
    with sqlite3.connect(delta_zip.parent / "history-delta.sqlite") as database:
        assert database.execute("SELECT COUNT(*) FROM repo_star_daily_delta").fetchone()[0] == 2


def test_canonical_star_event_is_not_filtered_out(tmp_path: Path) -> None:
    source = tmp_path / "canonical.parquet"
    _canonical_parquet(source)
    options = DuckDBOptions(temp_directory=tmp_path / "spill", memory_limit="1GB", threads=1)
    snapshot_zip = build_snapshot(
        [str(source)], tmp_path / "out", "canonical-v1", "2026-08-25", options, [7]
    )
    manifest = json.loads((snapshot_zip.parent / "manifest.json").read_text())
    assert manifest["repositories"] == 1
    assert manifest["watch_events"] == 2


def test_silver_can_rebuild_snapshot(tmp_path: Path) -> None:
    source = tmp_path / "watch.parquet"
    _parquet(source)
    options = DuckDBOptions(temp_directory=tmp_path / "spill", memory_limit="1GB", threads=1)
    silver = build_silver([str(source)], tmp_path / "silver-out", "silver-v1", "2026-08-25", options)
    manifest = json.loads((silver / "manifest.json").read_text())
    assert manifest["repositories"] == 2
    assert manifest["event_days"] == 3
    snapshot_zip = build_snapshot(
        [str(silver / "data" / "**" / "*.parquet")],
        tmp_path / "snapshot-out",
        "from-silver-v1",
        "2026-08-25",
        options,
    )
    snapshot_manifest = json.loads((snapshot_zip.parent / "manifest.json").read_text())
    assert snapshot_manifest["watch_events"] == 4
