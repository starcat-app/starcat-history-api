"""从 Parquet 流式构建 History Snapshot 与 Delta。

DuckDB 负责大规模过滤、聚合和外部排序；Python 只按 repo_id 顺序编码一条仓库序列，
不会把 2 亿级 repo/day 结果整体装入内存。
"""

from __future__ import annotations

import glob
import hashlib
import json
import os
import shutil
import sqlite3
import tempfile
import zipfile
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import duckdb

from .codec import ENCODING, encode
from .schema import DELTA_SCHEMA, SNAPSHOT_SCHEMA


@dataclass(frozen=True)
class DuckDBOptions:
    """限制本地任务资源，临时文件必须显式落到大容量数据盘。"""

    temp_directory: Path
    memory_limit: str = "12GB"
    threads: int = 4


def _resolve_inputs(patterns: list[str]) -> list[str]:
    paths: list[str] = []
    for pattern in patterns:
        matches = sorted(glob.glob(pattern, recursive=True))
        if not matches and Path(pattern).is_file():
            matches = [pattern]
        paths.extend(matches)
    unique = list(dict.fromkeys(str(Path(path).resolve()) for path in paths))
    if not unique:
        raise ValueError("没有匹配到任何 Parquet 输入文件")
    return unique


def input_checksum(patterns: list[str]) -> str:
    """计算输入集合的稳定内容摘要，供增量任务审计与安全重放。"""
    digest = hashlib.sha256()
    for path in _resolve_inputs(patterns):
        # 只纳入内容摘要，不写入机器相关的绝对路径，保证产物可在不同机器间核验。
        digest.update(_sha256(Path(path)).encode("ascii"))
        digest.update(b"\n")
    return digest.hexdigest()


def file_checksum(path: Path) -> str:
    """返回单个文件的 SHA-256，供产物完整性校验复用。"""
    return _sha256(path)


def _connect(options: DuckDBOptions) -> duckdb.DuckDBPyConnection:
    options.temp_directory.mkdir(parents=True, exist_ok=True)
    connection = duckdb.connect()
    # temp_directory 是防止全量排序占满系统盘的关键约束，不能退回默认目录。
    connection.execute("SET temp_directory = ?", [str(options.temp_directory.resolve())])
    connection.execute("SET memory_limit = ?", [options.memory_limit])
    connection.execute(f"SET threads = {max(1, options.threads)}")
    connection.execute("SET preserve_insertion_order = false")
    return connection


def _input_shape(connection: duckdb.DuckDBPyConnection, inputs: list[str]) -> tuple[str | None, bool]:
    columns = {row[0] for row in connection.execute("DESCRIBE SELECT * FROM read_parquet(?, union_by_name=true)", [inputs]).fetchall()}
    if "repo_id" not in columns:
        raise ValueError("输入必须包含 repo_id")
    if {"event_day", "event_count"}.issubset(columns):
        return None, False
    timestamp = "occurred_at" if "occurred_at" in columns else "created_at" if "created_at" in columns else ""
    if not timestamp:
        raise ValueError("输入必须包含 event_day/event_count 或 occurred_at/created_at")
    return timestamp, "relation_type" in columns


def _aggregate_sql(
    timestamp: str | None,
    has_relation_type: bool,
    repo_ids: list[int],
    *,
    from_exclusive: str | None = None,
    to_inclusive: str | None = None,
    ordered: bool = True,
) -> tuple[str, list[Any]]:
    if timestamp is None:
        where = ["repo_id IS NOT NULL", "repo_id > 0", "event_day IS NOT NULL", "event_count > 0"]
        parameters: list[Any] = []
        if repo_ids:
            placeholders = ",".join("?" for _ in repo_ids)
            where.append(f"repo_id IN ({placeholders})")
            parameters.extend(repo_ids)
        if from_exclusive:
            where.append("event_day > date_diff('day', DATE '1970-01-01', CAST(? AS DATE))")
            parameters.append(from_exclusive)
        if to_inclusive:
            where.append("event_day <= date_diff('day', DATE '1970-01-01', CAST(? AS DATE))")
            parameters.append(to_inclusive)
        order_clause = "ORDER BY repo_id, event_day" if ordered else ""
        return f"""
            SELECT CAST(repo_id AS BIGINT) AS repo_id,
                   CAST(event_day AS INTEGER) AS event_day,
                   CAST(SUM(event_count) AS BIGINT) AS event_count
            FROM read_parquet(?, union_by_name=true, hive_partitioning=true)
            WHERE {' AND '.join(where)}
            GROUP BY repo_id, event_day
            {order_clause}
        """, parameters

    where = ["repo_id IS NOT NULL", "repo_id > 0", f"{timestamp} IS NOT NULL"]
    parameters: list[Any] = []
    if has_relation_type:
        # Raw WatchEvent 没有 relation_type；Trainer Canonical 当前规范值是 star_event。
        # 同时接受旧实验数据的 watch，避免同一语义因字段枚举差异被静默过滤为空。
        where.append("relation_type IN ('watch', 'star_event')")
    if repo_ids:
        placeholders = ",".join("?" for _ in repo_ids)
        where.append(f"repo_id IN ({placeholders})")
        parameters.extend(repo_ids)
    utc_date = f"CAST({timestamp} AT TIME ZONE 'UTC' AS DATE)"
    if from_exclusive:
        where.append(f"{utc_date} > CAST(? AS DATE)")
        parameters.append(from_exclusive)
    if to_inclusive:
        where.append(f"{utc_date} <= CAST(? AS DATE)")
        parameters.append(to_inclusive)
    order_clause = "ORDER BY repo_id, event_day" if ordered else ""
    sql = f"""
        SELECT
            CAST(repo_id AS BIGINT) AS repo_id,
            CAST(date_diff('day', DATE '1970-01-01', {utc_date}) AS INTEGER) AS event_day,
            CAST(COUNT(*) AS BIGINT) AS event_count
        FROM read_parquet(?, union_by_name=true)
        WHERE {' AND '.join(where)}
        GROUP BY repo_id, event_day
        {order_clause}
    """
    return sql, parameters


def _prepare_destination(output_dir: Path, identifier: str) -> tuple[Path, Path]:
    final = output_dir.resolve() / identifier
    if final.exists():
        raise FileExistsError(f"产物目录已存在，拒绝覆盖: {final}")
    output_dir.mkdir(parents=True, exist_ok=True)
    staging = Path(tempfile.mkdtemp(prefix=f".{identifier}-", dir=output_dir))
    return staging, final


def _configure_sqlite(database: sqlite3.Connection) -> None:
    database.execute("PRAGMA journal_mode=OFF")
    database.execute("PRAGMA synchronous=OFF")
    database.execute("PRAGMA temp_store=MEMORY")
    database.execute("PRAGMA cache_size=-262144")


def build_snapshot(
    inputs: list[str],
    output_dir: Path,
    model_version: str,
    watermark: str,
    duckdb_options: DuckDBOptions,
    repo_ids: list[int] | None = None,
) -> Path:
    """构建完整或定向 Snapshot，返回可上传 ZIP 路径。"""
    resolved = _resolve_inputs(inputs)
    repo_ids = sorted(set(repo_ids or []))
    staging, final = _prepare_destination(output_dir, model_version)
    connection: duckdb.DuckDBPyConnection | None = None
    database: sqlite3.Connection | None = None
    try:
        connection = _connect(duckdb_options)
        timestamp, has_relation_type = _input_shape(connection, resolved)
        sql, parameters = _aggregate_sql(timestamp, has_relation_type, repo_ids, to_inclusive=watermark)
        cursor = connection.execute(sql, [resolved, *parameters])

        database_path = staging / "history.sqlite"
        database = sqlite3.connect(database_path)
        _configure_sqlite(database)
        database.executescript(SNAPSHOT_SCHEMA)
        insert = """
            INSERT INTO repo_history_series (
                repo_id, coverage_start_day, coverage_end_day, event_total, point_count,
                encoding, series, source_watermark, series_checksum
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
        """
        repository_count = point_count = watch_event_count = 0
        current_repo: int | None = None
        current_points: list[tuple[int, int]] = []
        pending: list[tuple[Any, ...]] = []

        def flush_repo() -> None:
            nonlocal repository_count, point_count, watch_event_count, current_points, pending
            if current_repo is None or not current_points:
                return
            payload, checksum, points, events = encode(current_points)
            pending.append((current_repo, current_points[0][0], current_points[-1][0], events, points, ENCODING, payload, watermark, checksum))
            repository_count += 1
            point_count += points
            watch_event_count += events
            current_points = []
            if len(pending) >= 10_000:
                database.executemany(insert, pending)
                database.commit()
                pending = []

        while rows := cursor.fetchmany(100_000):
            for repo_id, event_day, event_count in rows:
                repo_id = int(repo_id)
                if current_repo is not None and repo_id != current_repo:
                    flush_repo()
                current_repo = repo_id
                current_points.append((int(event_day), int(event_count)))
        flush_repo()
        if pending:
            database.executemany(insert, pending)
        generated_at = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
        database.execute(
            "INSERT INTO history_active (id, model_version, active_watermark, generated_at) VALUES (1, ?, ?, ?)",
            (model_version, watermark, generated_at),
        )
        database.commit()
        if database.execute("PRAGMA quick_check").fetchone()[0] != "ok":
            raise RuntimeError("Snapshot SQLite quick_check 未通过")
        database.close()
        database = None
        database_bytes = database_path.stat().st_size
        manifest = {
            "schema_version": 2,
            "kind": "history_snapshot",
            "model_version": model_version,
            "source_watermark": watermark,
            "created_at": generated_at,
            "repositories": repository_count,
            "event_days": point_count,
            "watch_events": watch_event_count,
            # 云端会继续校验 SHA-256、SQLite 表结构与激活水位；完整 quick_check
            # 只在 Builder 执行一次，避免 GiB 级数据库在发布端被重复全盘扫描。
            "validation": {
                "sqlite_quick_check": "ok",
                "database_bytes": database_bytes,
            },
        }
        _finish_bundle(staging, final, manifest, "history.sqlite", f"{model_version}.zip")
        return final / f"{model_version}.zip"
    except Exception:
        shutil.rmtree(staging, ignore_errors=True)
        raise
    finally:
        if database is not None:
            database.close()
        if connection is not None:
            connection.close()


def build_delta(
    inputs: list[str],
    output_dir: Path,
    delta_id: str,
    from_watermark: str,
    to_watermark: str,
    duckdb_options: DuckDBOptions,
) -> Path:
    """构建一个相邻水位的 repo/day Delta。"""
    resolved = _resolve_inputs(inputs)
    source_checksum = input_checksum(resolved)
    staging, final = _prepare_destination(output_dir, delta_id)
    connection: duckdb.DuckDBPyConnection | None = None
    database: sqlite3.Connection | None = None
    try:
        connection = _connect(duckdb_options)
        timestamp, has_relation_type = _input_shape(connection, resolved)
        sql, parameters = _aggregate_sql(
            timestamp,
            has_relation_type,
            [],
            from_exclusive=from_watermark,
            to_inclusive=to_watermark,
        )
        cursor = connection.execute(sql, [resolved, *parameters])
        database = sqlite3.connect(staging / "history-delta.sqlite")
        _configure_sqlite(database)
        database.executescript(DELTA_SCHEMA)
        row_count = 0
        while rows := cursor.fetchmany(100_000):
            normalized = [(int(repo_id), int(day), int(count)) for repo_id, day, count in rows]
            database.executemany("INSERT INTO repo_star_daily_delta VALUES (?, ?, ?)", normalized)
            row_count += len(normalized)
        database.commit()
        if database.execute("PRAGMA quick_check").fetchone()[0] != "ok":
            raise RuntimeError("Delta SQLite quick_check 未通过")
        database.close()
        database = None
        generated_at = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
        manifest = {
            "schema_version": 1,
            "kind": "history_delta",
            "delta_id": delta_id,
            "from_watermark": from_watermark,
            "to_watermark": to_watermark,
            "created_at": generated_at,
            "rows": row_count,
            "source_checksum": source_checksum,
        }
        _finish_bundle(staging, final, manifest, "history-delta.sqlite", f"{delta_id}.zip")
        return final / f"{delta_id}.zip"
    except Exception:
        shutil.rmtree(staging, ignore_errors=True)
        raise
    finally:
        if database is not None:
            database.close()
        if connection is not None:
            connection.close()


def _finish_bundle(staging: Path, final: Path, manifest: dict[str, Any], database_name: str, zip_name: str) -> None:
    manifest_path = staging / "manifest.json"
    manifest_path.write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    checksums = {
        "manifest.json": _sha256(manifest_path),
        database_name: _sha256(staging / database_name),
    }
    (staging / "checksums.json").write_text(json.dumps(checksums, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    archive_path = staging / zip_name
    with zipfile.ZipFile(archive_path, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=6) as archive:
        for name in ("manifest.json", "checksums.json", database_name):
            archive.write(staging / name, arcname=name)
    os.replace(staging, final)


def build_silver(
    inputs: list[str],
    output_dir: Path,
    dataset_id: str,
    watermark: str,
    duckdb_options: DuckDBOptions,
) -> Path:
    """把 Raw/Canonical 聚合为按年份分区的 repo/day Silver Parquet Dataset。"""
    resolved = _resolve_inputs(inputs)
    source_checksum = input_checksum(resolved)
    staging, final = _prepare_destination(output_dir, dataset_id)
    connection: duckdb.DuckDBPyConnection | None = None
    try:
        connection = _connect(duckdb_options)
        timestamp, has_relation_type = _input_shape(connection, resolved)
        if timestamp is None:
            raise ValueError("Silver 构建输入必须是 Raw/Canonical，不能再次输入 Silver")
        aggregate_sql, parameters = _aggregate_sql(
            timestamp,
            has_relation_type,
            [],
            to_inclusive=watermark,
            ordered=False,
        )
        data_directory = staging / "data"
        data_directory.mkdir()
        # 先聚合再按事件年份分区。年份分区既便于逐年审计，也避免生成数千个日目录。
        output_literal = str(data_directory).replace("'", "''")
        copy_sql = f"""
            COPY (
                SELECT repo_id, event_day, event_count,
                       year(DATE '1970-01-01' + event_day) AS event_year
                FROM ({aggregate_sql})
            ) TO '{output_literal}' (
                FORMAT PARQUET,
                COMPRESSION ZSTD,
                PARTITION_BY (event_year),
                ROW_GROUP_SIZE 100000,
                FILENAME_PATTERN 'part_{{uuid}}'
            )
        """
        # DuckDB COPY 的目标路径不能使用 prepared parameter；路径先做 SQL literal 转义。
        connection.execute(copy_sql, [resolved, *parameters])
        # COPY 的文件名契约固定为 part_{uuid}.parquet。必须先在 Python 侧按此前缀收窄，
        # 因为 macOS 向 exFAT 写扩展属性时会生成同扩展名的 ._part_* AppleDouble 文件；
        # DuckDB 不会忽略 glob 中的伪 Parquet，而是会因缺少 PAR1 footer 终止整次构建。
        files = sorted(data_directory.rglob("part_*.parquet"))
        rows, watch_events, repositories, minimum_day, maximum_day = connection.execute(
            """
            SELECT COUNT(*), COALESCE(SUM(event_count), 0), COUNT(DISTINCT repo_id),
                   MIN(event_day), MAX(event_day)
            FROM read_parquet(?, hive_partitioning=true)
            """,
            [[str(path) for path in files]],
        ).fetchone()
        generated_at = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
        manifest = {
            "schema_version": 1,
            "kind": "history_silver",
            "dataset_id": dataset_id,
            "source_watermark": watermark,
            "created_at": generated_at,
            "input_files": len(resolved),
            "input_checksum": source_checksum,
            "repositories": int(repositories),
            "event_days": int(rows),
            "watch_events": int(watch_events),
            "minimum_event_day": int(minimum_day) if minimum_day is not None else None,
            "maximum_event_day": int(maximum_day) if maximum_day is not None else None,
            "files": [
                {
                    "path": str(path.relative_to(staging)),
                    "bytes": path.stat().st_size,
                    "sha256": _sha256(path),
                }
                for path in files
            ],
        }
        (staging / "manifest.json").write_text(
            json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
        )
        os.replace(staging, final)
        return final
    except Exception:
        shutil.rmtree(staging, ignore_errors=True)
        raise
    finally:
        if connection is not None:
            connection.close()


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(8 * 1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()
