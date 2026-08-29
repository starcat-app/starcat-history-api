"""Starcat History Builder 命令行入口。"""

from __future__ import annotations

import argparse
from pathlib import Path

from .build import DuckDBOptions, build_delta, build_silver, build_snapshot


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
    return root


def main() -> None:
    args = parser().parse_args()
    options = DuckDBOptions(temp_directory=args.temp_dir, memory_limit=args.memory_limit, threads=args.threads)
    if args.command == "snapshot":
        output = build_snapshot(args.input, args.output_dir, args.model_version, args.watermark, options, args.repo_id)
    elif args.command == "silver":
        output = build_silver(args.input, args.output_dir, args.dataset_id, args.watermark, options)
    else:
        output = build_delta(args.input, args.output_dir, args.delta_id, args.from_watermark, args.to_watermark, options)
    print(output)


if __name__ == "__main__":
    main()
