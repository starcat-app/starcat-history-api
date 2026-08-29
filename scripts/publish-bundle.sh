#!/usr/bin/env bash
# 发布已经由 Builder 生成并校验的 Snapshot / Delta。
# Key 只从环境变量读取，避免出现在命令历史和进程参数中。
set -euo pipefail

usage() {
  echo "usage: $0 snapshot <version> <zip> <activate:true|false>" >&2
  echo "       $0 delta <delta-id> <zip>" >&2
}

if [[ $# -lt 3 ]]; then
  usage
  exit 2
fi

: "${HISTORY_API_BASE_URL:?HISTORY_API_BASE_URL is required}"
: "${HISTORY_PUBLISH_KEY:?HISTORY_PUBLISH_KEY is required}"

kind="$1"
identifier="$2"
bundle="$3"
if [[ ! -f "$bundle" ]]; then
  echo "bundle not found: $bundle" >&2
  exit 2
fi

headers=(
  -H "Authorization: Bearer ${HISTORY_PUBLISH_KEY}"
  -H "Content-Type: application/zip"
)
if [[ -n "${HISTORY_GATEWAY_SERVICE:-}" ]]; then
  headers+=(-H "X-SC-Svc: ${HISTORY_GATEWAY_SERVICE}")
fi

base="${HISTORY_API_BASE_URL%/}"
case "$kind" in
  snapshot)
    if [[ $# -ne 4 || "$4" != "true" && "$4" != "false" ]]; then
      usage
      exit 2
    fi
    target="${base}/internal/v1/history-snapshots/${identifier}?activate=$4"
    ;;
  delta)
    if [[ $# -ne 3 ]]; then
      usage
      exit 2
    fi
    target="${base}/internal/v1/history-deltas/${identifier}"
    ;;
  *)
    usage
    exit 2
    ;;
esac

curl --fail --silent --show-error \
  --request POST \
  "${headers[@]}" \
  --data-binary "@${bundle}" \
  "$target"

