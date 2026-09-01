#!/usr/bin/env python3
"""本地离线排查：某个仓库的 WatchEvent 在 Raw / Silver / Serving / App DB 各层是否存在。

对齐手工挖 tailscale/tailcat 的逻辑：只读本地文件，不打 GitHub / history-api。

用法（在 builder 目录，复用已有 DuckDB 依赖）：

    cd supports/starcat-history-api/builder
    uv run python scripts/probe_watch_events.py owner/repo
    uv run python scripts/probe_watch_events.py owner/repo --repo-id 880414192
    uv run python scripts/probe_watch_events.py owner/repo --skip-raw
    uv run python scripts/probe_watch_events.py owner/repo --nearby 500

改路径 / 水位 / 用户库：只改下方「可改常量」区块。
"""

from __future__ import annotations

import argparse
import sqlite3
import sys
from dataclasses import dataclass
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

import duckdb

# =============================================================================
# 可改常量 —— 换机器、账户或数据集时只改这里
# =============================================================================

# Starcat 当前账户的 GitHub user ID，用来定位该账户的本地 SQLite。
STARCAT_USER_ID = "20341123"

# 本地数据湖公共根目录；Raw、Silver 与 DuckDB 临时文件均由它派生。
STARCAT_DATA_ROOT = Path("/Volumes/T0/Starcat")

# 当前用于排查的完整 Silver 数据集。
SILVER_DATASET_ID = "watch-silver-2016-20260825-v1"

# Raw / Silver 水位（与下载 manifest 一致；扫描默认不超过此日）。
COVERAGE_END = date(2026, 8, 25)

# Raw 全库最早日（仅当本地库没有 created_at、且未传 --repo-id 起算日时作兜底）。
COVERAGE_START_FALLBACK = date(2016, 1, 1)

# 邻域对照：同窗内 [repo_id-N, repo_id+N] 的命中（说明 Raw 可读）。
NEARBY_RADIUS = 500

# DuckDB 资源（大扫 Raw 时按机器改）。
DUCKDB_MEMORY_LIMIT = "8GB"
DUCKDB_THREADS = 6

# =============================================================================
# 派生路径 —— 不要在这里写用户名、仓库位置或数据盘绝对路径
# =============================================================================

# 脚本固定在 <repository>/builder/scripts 下，因此仓库移动后仍能自动定位 Serving DB。
REPOSITORY_ROOT = Path(__file__).resolve().parents[2]
APP_SUPPORT_ROOT = Path.home() / "Library/Application Support/com.starcat.app"
HISTORY_DATA_ROOT = STARCAT_DATA_ROOT / "history"

# Starcat 本机用户库（用来把 owner/repo → repo_id，并看本地洞察点）。
APP_DB_PATH = APP_SUPPORT_ROOT / "users" / STARCAT_USER_ID / "starcat.sqlite"

# history-api Serving 快照。
SERVING_DB_PATH = REPOSITORY_ROOT / "data/history.sqlite"

# GH Archive WatchEvent Raw（按日 parquet：watch-events-YYYYMMDD.parquet）
RAW_WATCH_DIR = (
    STARCAT_DATA_ROOT / "bigquery/watch-events-2016-2026/raw/gh_archive"
)
RAW_FILE_GLOB = "watch-events-*.parquet"

# History Silver（event_year=*/part_*.parquet）
SILVER_DATA_DIR = HISTORY_DATA_ROOT / "silver" / SILVER_DATASET_ID / "data"
DUCKDB_TEMP_DIR = HISTORY_DATA_ROOT / "tmp"

# =============================================================================


@dataclass(frozen=True)
class AppRepo:
    repo_id: int
    full_name: str
    stars_count: int
    created_at: date | None
    is_my_project: bool


def _day_to_date(day: int) -> date:
    """Serving / Silver 的 day 编号 = 距 1970-01-01 的 UTC 天数。"""
    return date(1970, 1, 1) + timedelta(days=int(day))


def _parse_iso_date(value: str | None) -> date | None:
    if not value:
        return None
    text = value.strip()
    if not text:
        return None
    # 兼容 2024-10-29T17:21:12Z
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        return datetime.fromisoformat(text).date()
    except ValueError:
        try:
            return date.fromisoformat(text[:10])
        except ValueError:
            return None


def _open_ro(path: Path) -> sqlite3.Connection:
    if not path.is_file():
        raise FileNotFoundError(f"找不到 SQLite：{path}")
    return sqlite3.connect(f"file:{path}?mode=ro", uri=True)


def _duck() -> duckdb.DuckDBPyConnection:
    con = duckdb.connect()
    con.execute(f"SET memory_limit='{DUCKDB_MEMORY_LIMIT}'")
    con.execute(f"SET threads={int(DUCKDB_THREADS)}")
    if DUCKDB_TEMP_DIR:
        DUCKDB_TEMP_DIR.mkdir(parents=True, exist_ok=True)
        con.execute(f"SET temp_directory='{DUCKDB_TEMP_DIR}'")
    return con


def _list_raw_files(start: date, end: date) -> list[Path]:
    if not RAW_WATCH_DIR.is_dir():
        raise FileNotFoundError(f"找不到 Raw 目录：{RAW_WATCH_DIR}")
    start_s = start.strftime("%Y%m%d")
    end_s = end.strftime("%Y%m%d")
    files: list[Path] = []
    for path in sorted(RAW_WATCH_DIR.glob(RAW_FILE_GLOB)):
        name = path.name
        if name.startswith("._"):
            continue
        # watch-events-YYYYMMDD.parquet
        stamp = name[len("watch-events-") : len("watch-events-") + 8]
        if len(stamp) != 8 or not stamp.isdigit():
            continue
        if start_s <= stamp <= end_s:
            files.append(path)
    return files


def _list_silver_files(start: date, end: date) -> list[Path]:
    if not SILVER_DATA_DIR.is_dir():
        raise FileNotFoundError(f"找不到 Silver 目录：{SILVER_DATA_DIR}")
    years = range(start.year, end.year + 1)
    files: list[Path] = []
    for year in years:
        year_dir = SILVER_DATA_DIR / f"event_year={year}"
        if not year_dir.is_dir():
            continue
        for path in sorted(year_dir.glob("part_*.parquet")):
            if path.name.startswith("._"):
                continue
            files.append(path)
    # 若按年目录没命中，退回全量（仍跳过 AppleDouble）
    if not files:
        files = sorted(
            p
            for p in SILVER_DATA_DIR.rglob("*.parquet")
            if p.is_file() and not p.name.startswith("._")
        )
    return files


def load_app_repo(full_name: str, repo_id_override: int | None) -> AppRepo | None:
    """从本机 Starcat DB 解析仓库；不存在时返回 None（可用 --repo-id 继续查湖）。"""
    if not APP_DB_PATH.is_file():
        print(f"[warn] App DB 不存在，跳过本地身份解析：{APP_DB_PATH}")
        if repo_id_override is None:
            return None
        return AppRepo(
            repo_id=repo_id_override,
            full_name=full_name,
            stars_count=-1,
            created_at=None,
            is_my_project=False,
        )

    con = _open_ro(APP_DB_PATH)
    try:
        row = con.execute(
            """
            SELECT id, full_name, stars_count, created_at
            FROM repos
            WHERE full_name = ?
            """,
            (full_name,),
        ).fetchone()
        if row is None and repo_id_override is not None:
            row = con.execute(
                """
                SELECT id, full_name, stars_count, created_at
                FROM repos
                WHERE id = ?
                """,
                (repo_id_override,),
            ).fetchone()
        if row is None:
            return None

        repo_id, name, stars, created = row
        if repo_id_override is not None and int(repo_id) != repo_id_override:
            print(
                f"[warn] --repo-id={repo_id_override} 与本地库 id={repo_id} 不一致，"
                f"后续湖查询以 --repo-id 为准"
            )
            repo_id = repo_id_override

        is_project = con.execute(
            "SELECT COUNT(*) FROM user_projects WHERE repo_id = ?",
            (row[0],),
        ).fetchone()[0]

        return AppRepo(
            repo_id=int(repo_id),
            full_name=str(name),
            stars_count=int(stars or 0),
            created_at=_parse_iso_date(created),
            is_my_project=bool(is_project),
        )
    finally:
        con.close()


def load_local_history_points(repo_id: int) -> list[tuple]:
    if not APP_DB_PATH.is_file():
        return []
    con = _open_ro(APP_DB_PATH)
    try:
        return con.execute(
            """
            SELECT source, precision, COUNT(*) AS n,
                   MIN(observed_on), MAX(observed_on),
                   MIN(stars_count), MAX(stars_count)
            FROM repo_star_history_points
            WHERE repo_id = ?
            GROUP BY source, precision
            ORDER BY source, precision
            """,
            (repo_id,),
        ).fetchall()
    except sqlite3.OperationalError as exc:
        print(f"[warn] 读本地历史点失败：{exc}")
        return []
    finally:
        con.close()


def query_serving(repo_id: int) -> dict | None:
    if not SERVING_DB_PATH.is_file():
        print(f"[warn] Serving DB 不存在：{SERVING_DB_PATH}")
        return None
    con = _open_ro(SERVING_DB_PATH)
    try:
        row = con.execute(
            """
            SELECT repo_id, coverage_start_day, coverage_end_day,
                   event_total, point_count, source_watermark
            FROM repo_history_series
            WHERE repo_id = ?
            """,
            (repo_id,),
        ).fetchone()
        if row is None:
            return None
        return {
            "repo_id": row[0],
            "coverage_start": _day_to_date(row[1]).isoformat(),
            "coverage_end": _day_to_date(row[2]).isoformat(),
            "event_total": row[3],
            "point_count": row[4],
            "source_watermark": row[5],
        }
    finally:
        con.close()


def query_raw(
    repo_id: int,
    start: date,
    end: date,
    nearby_radius: int,
) -> dict:
    files = _list_raw_files(start, end)
    if not files:
        return {
            "files": 0,
            "rows": 0,
            "min_created_at": None,
            "max_created_at": None,
            "nearby_rows": 0,
            "nearby_repos": 0,
            "nearby_top": [],
            "window": (start.isoformat(), end.isoformat()),
        }

    con = _duck()
    paths = [str(p) for p in files]
    try:
        rows, min_at, max_at = con.execute(
            """
            SELECT COUNT(*) AS rows,
                   MIN(created_at) AS min_created_at,
                   MAX(created_at) AS max_created_at
            FROM read_parquet(?)
            WHERE repo_id = ?
            """,
            [paths, repo_id],
        ).fetchone()

        nearby_rows = nearby_repos = 0
        nearby_top: list[tuple] = []
        if nearby_radius > 0:
            lo = repo_id - nearby_radius
            hi = repo_id + nearby_radius
            nearby_rows, nearby_repos = con.execute(
                """
                SELECT COUNT(*) AS rows, COUNT(DISTINCT repo_id) AS repos
                FROM read_parquet(?)
                WHERE repo_id BETWEEN ? AND ?
                """,
                [paths, lo, hi],
            ).fetchone()
            nearby_top = con.execute(
                """
                SELECT repo_id, COUNT(*) AS n
                FROM read_parquet(?)
                WHERE repo_id BETWEEN ? AND ?
                GROUP BY 1
                ORDER BY n DESC
                LIMIT 5
                """,
                [paths, lo, hi],
            ).fetchall()

        return {
            "files": len(files),
            "first_file": files[0].name,
            "last_file": files[-1].name,
            "rows": int(rows or 0),
            "min_created_at": str(min_at) if min_at is not None else None,
            "max_created_at": str(max_at) if max_at is not None else None,
            "nearby_rows": int(nearby_rows or 0),
            "nearby_repos": int(nearby_repos or 0),
            "nearby_top": nearby_top,
            "window": (start.isoformat(), end.isoformat()),
        }
    finally:
        con.close()


def query_silver(repo_id: int, start: date, end: date) -> dict:
    files = _list_silver_files(start, end)
    if not files:
        return {
            "files": 0,
            "rows": 0,
            "events": 0,
            "min_day": None,
            "max_day": None,
        }

    con = _duck()
    paths = [str(p) for p in files]
    try:
        rows, events, min_day, max_day = con.execute(
            """
            SELECT COUNT(*) AS rows,
                   COALESCE(SUM(event_count), 0) AS events,
                   MIN(event_day) AS min_day,
                   MAX(event_day) AS max_day
            FROM read_parquet(?, union_by_name=true)
            WHERE repo_id = ?
            """,
            [paths, repo_id],
        ).fetchone()
        return {
            "files": len(files),
            "rows": int(rows or 0),
            "events": int(events or 0),
            "min_day": _day_to_date(min_day).isoformat() if min_day is not None else None,
            "max_day": _day_to_date(max_day).isoformat() if max_day is not None else None,
        }
    finally:
        con.close()


def _print_section(title: str) -> None:
    print()
    print(f"=== {title} ===")


def run(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="本地排查仓库 WatchEvent：App DB / Serving / Raw / Silver（不走网络）"
    )
    parser.add_argument(
        "full_name",
        help="GitHub full_name，例如 tailscale/tailcat",
    )
    parser.add_argument(
        "--repo-id",
        type=int,
        default=None,
        help="强制使用该 repo_id（本地库没有该仓时必填）",
    )
    parser.add_argument(
        "--from",
        dest="date_from",
        default=None,
        help="Raw/Silver 扫描起始日 YYYY-MM-DD（默认：仓库 created_at 或常量兜底）",
    )
    parser.add_argument(
        "--to",
        dest="date_to",
        default=None,
        help=f"Raw/Silver 扫描结束日 YYYY-MM-DD（默认：{COVERAGE_END.isoformat()}）",
    )
    parser.add_argument(
        "--nearby",
        type=int,
        default=NEARBY_RADIUS,
        help=f"Raw 邻域半径（默认 {NEARBY_RADIUS}；0=关闭）",
    )
    parser.add_argument("--skip-raw", action="store_true", help="跳过 Raw 扫描（最快）")
    parser.add_argument("--skip-silver", action="store_true", help="跳过 Silver 扫描")
    args = parser.parse_args(argv)

    full_name = args.full_name.strip().strip("/")
    if full_name.count("/") != 1:
        print(f"错误：full_name 必须是 owner/repo，收到：{args.full_name!r}", file=sys.stderr)
        return 2

    print("probe_watch_events — 本地离线排查（无网络）")
    print(f"full_name     : {full_name}")
    print(f"APP_DB        : {APP_DB_PATH}")
    print(f"SERVING_DB    : {SERVING_DB_PATH}")
    print(f"RAW_DIR       : {RAW_WATCH_DIR}")
    print(f"SILVER_DIR    : {SILVER_DATA_DIR}")
    print(f"COVERAGE_END  : {COVERAGE_END.isoformat()}")

    app = load_app_repo(full_name, args.repo_id)
    _print_section("1. App / 身份")
    if app is None:
        if args.repo_id is None:
            print("本地库无此仓，且未传 --repo-id，无法继续。")
            print("示例：uv run python scripts/probe_watch_events.py owner/repo --repo-id 123")
            return 1
        repo_id = args.repo_id
        created = None
        print(f"本地库无此仓；使用 --repo-id={repo_id}")
        is_my_project = False
        stars = -1
    else:
        repo_id = app.repo_id
        created = app.created_at
        is_my_project = app.is_my_project
        stars = app.stars_count
        print(f"repo_id       : {app.repo_id}")
        print(f"full_name     : {app.full_name}")
        print(f"stars_count   : {app.stars_count}")
        print(f"created_at    : {app.created_at.isoformat() if app.created_at else '(unknown)'}")
        print(f"is_my_project : {app.is_my_project}")
        if app.is_my_project:
            print("提示：我的项目 → 客户端洞察默认走 Stargazers，不打 history-api。")

        points = load_local_history_points(app.repo_id)
        if points:
            print("local history points:")
            for source, precision, n, dmin, dmax, smin, smax in points:
                print(
                    f"  - {source}/{precision}: n={n} "
                    f"dates={dmin}..{dmax} stars={smin}..{smax}"
                )
        else:
            print("local history points: (none)")

    end = _parse_iso_date(args.date_to) or COVERAGE_END
    start = _parse_iso_date(args.date_from) or created or COVERAGE_START_FALLBACK
    if start > end:
        print(f"错误：起始日 {start} 晚于结束日 {end}", file=sys.stderr)
        return 2

    _print_section("2. Serving")
    serving = query_serving(repo_id)
    if serving is None:
        print("无此 repo_id → history-api 会返回 HISTORY_NOT_FOUND")
    else:
        for key, value in serving.items():
            print(f"{key:18}: {value}")

    raw_result = None
    if args.skip_raw:
        _print_section("3. Raw WatchEvent")
        print("已跳过 (--skip-raw)")
    else:
        _print_section("3. Raw WatchEvent")
        print(f"window        : {start.isoformat()} → {end.isoformat()}")
        print(f"nearby_radius : {args.nearby}")
        t0 = datetime.now(timezone.utc)
        raw_result = query_raw(repo_id, start, end, args.nearby)
        elapsed = (datetime.now(timezone.utc) - t0).total_seconds()
        print(f"files         : {raw_result['files']}")
        if raw_result["files"]:
            print(f"first/last    : {raw_result['first_file']} .. {raw_result['last_file']}")
        print(f"rows          : {raw_result['rows']}")
        print(f"min_created   : {raw_result['min_created_at']}")
        print(f"max_created   : {raw_result['max_created_at']}")
        if args.nearby > 0:
            print(
                f"nearby        : rows={raw_result['nearby_rows']} "
                f"repos={raw_result['nearby_repos']}"
            )
            for rid, n in raw_result["nearby_top"]:
                mark = " ← target" if int(rid) == repo_id else ""
                print(f"  - repo_id={rid} n={n}{mark}")
        print(f"elapsed       : {elapsed:.1f}s")

    silver_result = None
    if args.skip_silver:
        _print_section("4. Silver")
        print("已跳过 (--skip-silver)")
    else:
        _print_section("4. Silver")
        print(f"years         : {start.year} → {end.year}")
        t0 = datetime.now(timezone.utc)
        silver_result = query_silver(repo_id, start, end)
        elapsed = (datetime.now(timezone.utc) - t0).total_seconds()
        print(f"files         : {silver_result['files']}")
        print(f"rows          : {silver_result['rows']}")
        print(f"events        : {silver_result['events']}")
        print(f"min_day       : {silver_result['min_day']}")
        print(f"max_day       : {silver_result['max_day']}")
        print(f"elapsed       : {elapsed:.1f}s")

    _print_section("5. 结论")
    raw_ok = raw_result is not None and raw_result["rows"] > 0
    silver_ok = silver_result is not None and silver_result["events"] > 0
    serving_ok = serving is not None and int(serving.get("event_total") or 0) > 0

    if serving_ok:
        print("Serving 有序列 → history-api /events 应能返回；客户端刷新应能落 gh_archive。")
    elif silver_ok and not serving_ok:
        print("Silver 有、Serving 无 → 快照/导入漏了这条仓，不是 Raw 源缺口。")
    elif raw_ok and not silver_ok:
        print("Raw 有、Silver 无 → Silver 构建漏聚合，不是 GH Archive 源缺口。")
    elif not raw_ok and raw_result is not None:
        print(
            "Raw 窗口内 0 事件 → 源 GH Archive WatchEvent 就没有这条仓；"
            "Silver/Serving 为空是连带结果，不是客户端 bug。"
        )
        if raw_result.get("nearby_rows", 0) > 0:
            print("邻域有命中 → Raw 文件可读、扫描逻辑正常，缺口针对该 repo_id。")
    else:
        print("未跑 Raw，或路径不可用；请根据上面各层输出自行判断。")

    if is_my_project:
        print("另：该仓是「我的项目」，即使 Serving 有数据，客户端默认也不会打 history-api。")
    if stars >= 0 and not raw_ok and raw_result is not None:
        print(f"对照：本地 stars_count={stars}，但 Raw WatchEvent=0（GH Archive 不完整或缺失）。")

    return 0


if __name__ == "__main__":
    raise SystemExit(run())
