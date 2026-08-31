#!/usr/bin/env bash
# 从本地已下载的 D-1 WatchEvent 分区构建 Silver/Delta，并幂等发布到 History Serving。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
builder_cli="${repo_root}/builder/.venv/bin/starcat-history-builder"
target_date="${1:-$(date -u -v-1d +%F)}"
compact_date="${target_date//-/}"
raw_root="${HISTORY_RAW_ROOT:-/Volumes/T0/Starcat/bigquery/watch-events-2016-2026/raw/gh_archive}"
history_root="${HISTORY_DATA_ROOT:-/Volumes/T0/Starcat/history}"
base_url="${HISTORY_BASE_URL:-http://127.0.0.1:5014}"
raw_file="${raw_root}/watch-events-${compact_date}.parquet"

if [[ ! -f "${raw_file}" ]]; then
  echo "缺少已下载的 WatchEvent 分区: ${raw_file}" >&2
  exit 2
fi
if [[ -z "${HISTORY_PUBLISH_KEY:-}" ]]; then
  echo "HISTORY_PUBLISH_KEY 未配置" >&2
  exit 2
fi
if [[ ! -x "${builder_cli}" ]]; then
  echo "History Builder 未安装，请先在 builder 目录执行 uv sync --extra test --python 3.12" >&2
  exit 2
fi

exec "${builder_cli}" daily \
  --input "${raw_file}" \
  --silver-dir "${history_root}/silver/daily" \
  --output-dir "${history_root}/deltas" \
  --temp-dir "${history_root}/spill" \
  --target-watermark "${target_date}" \
  --base-url "${base_url}" \
  --gateway-service "${HISTORY_GATEWAY_SERVICE:-}" \
  --publish-key-env HISTORY_PUBLISH_KEY \
  --memory-limit "${HISTORY_MEMORY_LIMIT:-12GB}" \
  --threads "${HISTORY_THREADS:-4}"
