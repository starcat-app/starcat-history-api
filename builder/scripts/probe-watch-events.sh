#!/usr/bin/env bash
# 本地离线排查仓库 WatchEvent（包装 uv + probe_watch_events.py）
#
# 用法：
#   ./scripts/probe-watch-events.sh owner/repo
#   ./scripts/probe-watch-events.sh owner/repo --skip-raw
#   ./scripts/probe-watch-events.sh owner/repo --repo-id 880414192
#
# 路径常量在 builder/scripts/probe_watch_events.py 顶部修改。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILDER_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

cd "$BUILDER_DIR"
exec uv run python scripts/probe_watch_events.py "$@"
