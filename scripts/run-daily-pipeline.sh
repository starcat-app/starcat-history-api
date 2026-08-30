#!/usr/bin/env bash
# 从本地已下载的 D-1 WatchEvent 分区构建 Silver/Delta，并幂等发布到 History Serving。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
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

cd "${repo_root}/builder"
exec uv run starcat-history-builder daily \
  --input "${raw_file}" \
  --silver-dir "${history_root}/silver/daily" \
  --output-dir "${history_root}/deltas" \
  --temp-dir "${history_root}/spill" \
  --target-watermark "${target_date}" \
  --base-url "${base_url}" \
  --publish-key-env HISTORY_PUBLISH_KEY \
  --memory-limit "${HISTORY_MEMORY_LIMIT:-12GB}" \
  --threads "${HISTORY_THREADS:-4}"
