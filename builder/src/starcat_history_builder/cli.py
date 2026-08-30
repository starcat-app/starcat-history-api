"""Starcat History Builder 命令行入口。"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path

from .build import DuckDBOptions, build_delta, build_silver, build_snapshot
from .daily import DailyOptions, HTTPHistoryPublisher, run_daily


def _common(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--input", action="append", required=True, help="Parquet 文件或 glob，可重复")
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--temp-dir", type=Path, required=True, help="DuckDB spill 目录，建议放在大容量数据盘")
    parser.add_argument("--memory-limit", default="12GB")
    parser.add_argument("--threads", type=int, default=4)


def parser() -> argparse.ArgumentParser:
    root = argparse.ArgumentParser(prog="starcat-history-builder")
    commands = root.add_subparsers(dest="command", required=True)
    snapshot = commands.add_parser("snapshot", help="构建完整或定向 Snapshot")
    _common(snapshot)
    snapshot.add_argument("--model-version", required=True)
    snapshot.add_argument("--watermark", required=True, help="YYYY-MM-DD")
    snapshot.add_argument("--repo-id", action="append", type=int, default=[], help="可重复；不传表示全量")
    silver = commands.add_parser("silver", help="从 Raw/Canonical 构建日级 Silver Dataset")
    _common(silver)
    silver.add_argument("--dataset-id", required=True)
    silver.add_argument("--watermark", required=True, help="YYYY-MM-DD")
    delta = commands.add_parser("delta", help="构建相邻水位 Delta")
    _common(delta)
    delta.add_argument("--delta-id", required=True)
    delta.add_argument("--from-watermark", required=True)
    delta.add_argument("--to-watermark", required=True)
    daily = commands.add_parser("daily", help="从单日 Raw 构建 Silver/Delta 并幂等发布")
    _common(daily)
    daily.add_argument("--silver-dir", type=Path, required=True)
    daily.add_argument("--target-watermark", required=True, help="YYYY-MM-DD，必须与服务端水位相邻")
    daily.add_argument("--base-url", required=True, help="History 服务地址，可包含聚合服务的 /history 前缀")
    daily.add_argument("--publish-key-env", default="HISTORY_PUBLISH_KEY")
    daily.add_argument("--timeout-seconds", type=int, default=600)
    return root


def main() -> None:
    args = parser().parse_args()
    options = DuckDBOptions(temp_directory=args.temp_dir, memory_limit=args.memory_limit, threads=args.threads)
    if args.command == "snapshot":
        output = build_snapshot(args.input, args.output_dir, args.model_version, args.watermark, options, args.repo_id)
    elif args.command == "silver":
        output = build_silver(args.input, args.output_dir, args.dataset_id, args.watermark, options)
    elif args.command == "delta":
        output = build_delta(args.input, args.output_dir, args.delta_id, args.from_watermark, args.to_watermark, options)
    else:
        token = os.environ.get(args.publish_key_env, "")
        if not token:
            raise RuntimeError(f"环境变量 {args.publish_key_env} 未配置")
        output = run_daily(
            DailyOptions(args.input, args.silver_dir, args.output_dir, args.target_watermark, options),
            HTTPHistoryPublisher(args.base_url, token, args.timeout_seconds),
        )
    print(json.dumps(output, ensure_ascii=False, indent=2) if isinstance(output, dict) else output)


if __name__ == "__main__":
    main()
