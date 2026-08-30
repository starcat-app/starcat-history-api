"""Builder 的最小真实 Parquet -> SQLite -> ZIP 验证。"""

from __future__ import annotations

import json
import sqlite3
import zipfile
from pathlib import Path

import duckdb

import starcat_history_builder.build as builder_module
from starcat_history_builder.build import DuckDBOptions, build_delta, build_silver, build_snapshot
from starcat_history_builder.daily import DailyOptions, run_daily


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


def _daily_parquet(path: Path) -> None:
    connection = duckdb.connect()
    connection.execute(
        """
        COPY (
            SELECT * FROM (VALUES
                ('c', 'u3', 7::BIGINT, TIMESTAMPTZ '2026-08-25 01:00:00+00'),
                ('d', 'u4', 8::BIGINT, TIMESTAMPTZ '2026-08-25 02:00:00+00')
            ) AS events(source_record_id, actor_id, repo_id, created_at)
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
    assert manifest["schema_version"] == 2
    assert manifest["repositories"] == 2
    assert manifest["event_days"] == 3
    assert manifest["watch_events"] == 4
    assert manifest["validation"]["sqlite_quick_check"] == "ok"
    assert manifest["validation"]["database_bytes"] == (snapshot_dir / "history.sqlite").stat().st_size
    with sqlite3.connect(snapshot_dir / "history.sqlite") as database:
        assert database.execute("SELECT point_count, event_total FROM repo_history_series WHERE repo_id=7").fetchone() == (2, 3)
        assert database.execute("PRAGMA quick_check").fetchone()[0] == "ok"
    with zipfile.ZipFile(snapshot_zip) as archive:
        assert sorted(archive.namelist()) == ["checksums.json", "history.sqlite", "manifest.json"]

    delta_zip = build_delta([str(source)], tmp_path / "delta-out", "delta-1", "2026-08-24", "2026-08-25", options)
    delta_manifest = json.loads((delta_zip.parent / "manifest.json").read_text())
    assert len(delta_manifest["source_checksum"]) == 64
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


def test_silver_ignores_appledouble_parquet_sidecars(tmp_path: Path, monkeypatch) -> None:
    source = tmp_path / "watch.parquet"
    _parquet(source)
    options = DuckDBOptions(temp_directory=tmp_path / "spill", memory_limit="1GB", threads=1)
    real_connect = builder_module._connect
    silver_output = tmp_path / "silver-out"

    class AppleDoubleInjectingConnection:
        """在 COPY 完成后模拟 macOS 向 exFAT 写入的 AppleDouble 伴生文件。"""

        def __init__(self, connection: duckdb.DuckDBPyConnection) -> None:
            self.connection = connection

        def execute(self, sql: str, parameters=None):
            result = (
                self.connection.execute(sql, parameters)
                if parameters is not None
                else self.connection.execute(sql)
            )
            if "FORMAT PARQUET" in sql and "PARTITION_BY" in sql:
                generated = list(silver_output.rglob("part_*.parquet"))
                assert generated
                for parquet in generated:
                    # AppleDouble 使用相同扩展名但不是 Parquet；宽泛 glob 不能把它交给 DuckDB。
                    parquet.with_name(f"._{parquet.name}").write_bytes(b"\x00\x05\x16\x07AppleDouble")
            return result

        def close(self) -> None:
            self.connection.close()

    monkeypatch.setattr(
        builder_module,
        "_connect",
        lambda configured_options: AppleDoubleInjectingConnection(real_connect(configured_options)),
    )

    silver = build_silver([str(source)], silver_output, "silver-v1", "2026-08-25", options)
    manifest = json.loads((silver / "manifest.json").read_text())

    assert manifest["watch_events"] == 4
    assert all(not file["path"].split("/")[-1].startswith("._") for file in manifest["files"])


def test_daily_pipeline_is_publishable_and_replay_safe(tmp_path: Path) -> None:
    source = tmp_path / "watch.parquet"
    _daily_parquet(source)
    options = DuckDBOptions(temp_directory=tmp_path / "spill", memory_limit="1GB", threads=1)

    class FakePublisher:
        """模拟服务端水位；重跑时由服务端事实直接判定已完成。"""

        def __init__(self) -> None:
            self.watermark = "2026-08-24"
            self.uploads = 0

        def active(self) -> dict[str, str]:
            return {"model_version": "fixture-v1", "active_watermark": self.watermark}

        def publish_delta(self, delta_id: str, archive: Path) -> dict[str, object]:
            assert delta_id == "watch-delta-20260825-v1"
            assert archive.is_file()
            with zipfile.ZipFile(archive) as bundle:
                manifest = json.loads(bundle.read("manifest.json"))
            assert manifest["source_checksum"]
            self.uploads += 1
            self.watermark = "2026-08-25"
            return {"delta_id": delta_id, "active_watermark": self.watermark, "applied": True}

    publisher = FakePublisher()
    daily = DailyOptions(
        [str(source)], tmp_path / "silver", tmp_path / "deltas", "2026-08-25", options
    )
    result = run_daily(daily, publisher)
    assert result["status"] == "published"
    assert publisher.uploads == 1
    assert (tmp_path / "deltas" / "watch-delta-20260825-v1" / "publish-receipt.json").is_file()

    replay = run_daily(daily, publisher)
    assert replay["status"] == "already_applied"
    assert publisher.uploads == 1
