#!/usr/bin/env bash
# 把 Builder 产出的 Snapshot SQLite 安装到本地 history-api 的 STORE_FILE。
#
# 适用场景：本机联调，不想走 publish ZIP / Registry 激活流程。
# 只需 history.sqlite；manifest.json / checksums.json / *.zip 不必拷贝。
#
# 关键约束：
# - 拷贝前应停止占用 STORE_FILE 的 history-api，避免半截 WAL；
# - 安装后服务走 bootstrap 模式读 STORE_FILE（history-registry 可为空）；
# - 后续日增量仍应使用 scripts/publish-bundle.sh。
set -euo pipefail

usage() {
  cat <<'EOF' >&2
usage: scripts/install-local-snapshot.sh <snapshot-dir|history.sqlite> [--force] [--allow-running]

examples:
  scripts/install-local-snapshot.sh \
    /Volumes/T0/Starcat/history/snapshots/watch-history-20260825-v1

  STORE_FILE=./data/history.sqlite \
    scripts/install-local-snapshot.sh ./path/to/history.sqlite --force

options:
  --force           覆盖已有 STORE_FILE
  --allow-running   允许在检测到监听端口时仍继续（不推荐）
EOF
}

if [[ $# -lt 1 ]]; then
  usage
  exit 2
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

SOURCE_ARG=""
FORCE=0
ALLOW_RUNNING=0
for arg in "$@"; do
  case "$arg" in
    --force) FORCE=1 ;;
    --allow-running) ALLOW_RUNNING=1 ;;
    -h|--help)
      usage
      exit 0
      ;;
    -*)
      echo "unknown option: $arg" >&2
      usage
      exit 2
      ;;
    *)
      if [[ -n "$SOURCE_ARG" ]]; then
        echo "unexpected argument: $arg" >&2
        usage
        exit 2
      fi
      SOURCE_ARG="$arg"
      ;;
  esac
done

if [[ -z "$SOURCE_ARG" ]]; then
  usage
  exit 2
fi

# 优先环境变量；否则从 .env 读 STORE_FILE / PORT；再退回默认值。
load_dotenv_value() {
  local key="$1"
  local file="$ROOT_DIR/.env"
  [[ -f "$file" ]] || return 0
  local line
  line="$(grep -E "^${key}=" "$file" | tail -n 1 || true)"
  [[ -n "$line" ]] || return 0
  printf '%s\n' "${line#*=}"
}

STORE_FILE="${STORE_FILE:-$(load_dotenv_value STORE_FILE)}"
STORE_FILE="${STORE_FILE:-./data/history.sqlite}"
PORT="${PORT:-$(load_dotenv_value PORT)}"
PORT="${PORT:-5014}"

resolve_source_sqlite() {
  local input="$1"
  if [[ -d "$input" ]]; then
    local candidate="$input/history.sqlite"
    if [[ ! -f "$candidate" ]]; then
      echo "snapshot directory missing history.sqlite: $input" >&2
      exit 2
    fi
    printf '%s\n' "$candidate"
    return
  fi
  if [[ -f "$input" ]]; then
    printf '%s\n' "$input"
    return
  fi
  echo "source not found: $input" >&2
  exit 2
}

SOURCE_SQLITE="$(resolve_source_sqlite "$SOURCE_ARG")"
SOURCE_SQLITE="$(cd "$(dirname "$SOURCE_SQLITE")" && pwd)/$(basename "$SOURCE_SQLITE")"

if [[ "$STORE_FILE" != /* ]]; then
  DEST_SQLITE="$ROOT_DIR/${STORE_FILE#./}"
else
  DEST_SQLITE="$STORE_FILE"
fi
DEST_DIR="$(dirname "$DEST_SQLITE")"

if command -v lsof >/dev/null 2>&1; then
  if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    if [[ "$ALLOW_RUNNING" -ne 1 ]]; then
      echo "history-api still listening on :$PORT; stop it first, or pass --allow-running" >&2
      lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >&2 || true
      exit 3
    fi
    echo "warning: history-api is listening on :$PORT; continuing because --allow-running was set" >&2
  fi
fi

if [[ ! -f "$SOURCE_SQLITE" ]]; then
  echo "source sqlite missing: $SOURCE_SQLITE" >&2
  exit 2
fi

if [[ -e "$DEST_SQLITE" && "$FORCE" -ne 1 ]]; then
  echo "destination already exists: $DEST_SQLITE (pass --force to overwrite)" >&2
  exit 2
fi

mkdir -p "$DEST_DIR"

# 轻量校验：必须能读到 Serving 核心表和 active 水位。
if ! command -v sqlite3 >/dev/null 2>&1; then
  echo "sqlite3 is required to verify the snapshot" >&2
  exit 2
fi

VERIFY_SQL=$'SELECT COUNT(*) FROM sqlite_master WHERE type=\'table\' AND name IN (\'repo_history_series\',\'history_active\');\nSELECT model_version || \'|\' || active_watermark FROM history_active WHERE id = 1;\nSELECT COUNT(*) FROM repo_history_series;'
VERIFY_OUT="$(sqlite3 -readonly "$SOURCE_SQLITE" "$VERIFY_SQL")"
TABLE_COUNT="$(printf '%s\n' "$VERIFY_OUT" | sed -n '1p')"
ACTIVE_LINE="$(printf '%s\n' "$VERIFY_OUT" | sed -n '2p')"
REPO_COUNT="$(printf '%s\n' "$VERIFY_OUT" | sed -n '3p')"

if [[ "$TABLE_COUNT" != "2" || -z "$ACTIVE_LINE" || -z "$REPO_COUNT" || "$REPO_COUNT" -le 0 ]]; then
  echo "source sqlite failed Serving checks: $SOURCE_SQLITE" >&2
  echo "$VERIFY_OUT" >&2
  exit 2
fi

echo "installing local snapshot"
echo "  source : $SOURCE_SQLITE"
echo "  dest   : $DEST_SQLITE"
echo "  active : $ACTIVE_LINE"
echo "  repos  : $REPO_COUNT"

# 覆盖前清掉旧 WAL，避免重启后旧事务盖住新库。
rm -f "${DEST_SQLITE}-wal" "${DEST_SQLITE}-shm"
TMP_DEST="${DEST_SQLITE}.tmp.$$"
trap 'rm -f "$TMP_DEST"' EXIT
cp -f "$SOURCE_SQLITE" "$TMP_DEST"
mv -f "$TMP_DEST" "$DEST_SQLITE"
trap - EXIT

# 再读一次目标库，确认拷贝完整。
DEST_ACTIVE="$(sqlite3 -readonly "$DEST_SQLITE" "SELECT model_version || '|' || active_watermark FROM history_active WHERE id = 1;")"
DEST_REPOS="$(sqlite3 -readonly "$DEST_SQLITE" "SELECT COUNT(*) FROM repo_history_series;")"
if [[ "$DEST_ACTIVE" != "$ACTIVE_LINE" || "$DEST_REPOS" != "$REPO_COUNT" ]]; then
  echo "destination verification failed after copy" >&2
  exit 1
fi

echo "done. start API with: make run"
echo "note: manifest.json / checksums.json / *.zip are not required for this bootstrap path."
