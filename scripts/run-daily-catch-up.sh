#!/usr/bin/env bash
# 从生产 History 水位开始，连续消费本地 WatchEvent 分区并发布到目标水位。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
target_date="${1:-$(date -u -v-1d +%F)}"
raw_root="${HISTORY_RAW_ROOT:-/Volumes/T0/Starcat/bigquery/watch-events-2016-2026/raw/gh_archive}"
history_root="${HISTORY_DATA_ROOT:-/Volumes/T0/Starcat/history}"
base_url="${HISTORY_BASE_URL:-http://127.0.0.1:5014}"

if [[ -z "${HISTORY_PUBLISH_KEY:-}" ]]; then
  echo "HISTORY_PUBLISH_KEY 未配置" >&2
  exit 2
fi

cd "${repo_root}/builder"
exec uv run starcat-history-builder catch-up \
  --raw-dir "${raw_root}" \
  --silver-dir "${history_root}/silver/daily" \
  --output-dir "${history_root}/deltas" \
  --temp-dir "${history_root}/spill" \
  --target-watermark "${target_date}" \
  --base-url "${base_url}" \
  --gateway-service "${HISTORY_GATEWAY_SERVICE:-}" \
  --publish-key-env HISTORY_PUBLISH_KEY \
  --memory-limit "${HISTORY_MEMORY_LIMIT:-12GB}" \
  --threads "${HISTORY_THREADS:-4}"
