#!/usr/bin/env bash
# ============================================================================
# starcat-history-api 压测与基线采集
#
# 用法:
#   scripts/bench-history-api.sh                                  # 打本机默认实例
#   scripts/bench-history-api.sh --base https://history.starcat.ink --key "$API_KEY"
#   scripts/bench-history-api.sh --cold some-org/some-repo        # 追加并发冷启动场景
#
# 为什么需要它：这个服务的对外成本几乎全在「回源 GitHub」上，而回源是否发生
# 无法从响应时间单看出来（命中内存缓存与命中 SQLite 都快，冷启动与"每次回源"
# 都很慢）。所以脚本同时采集两样东西：
#   1) 延迟分位（curl time_total）
#   2) /internal/metrics/service 的计数差值（回源次数、缓存命中）—— 需要 --key
#
# 四个场景对应四类真实压力：
#   warm-svg     README 图片的标准热路径
#   warm-json    第三方站点渲染曲线（默认不带 repo_id，是最容易被写成"每次回源"的入口）
#   404-flood    不存在的仓库被反复抓取（验证负缓存是否兜住 GitHub 调用）
#   cold-burst   同一仓库并发冷启动（验证请求合并：N 个并发只应产生 1 次回源）
# ============================================================================

set -euo pipefail

base="http://127.0.0.1:5014"
repo="starcat-app/Starcat"
missing="starcat-app/bench-missing-repo-0000"
cold=""
key="${STARCAT_API_KEY:-}"
requests=30
concurrency=10
timeout=45

usage() {
    sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'
    exit "${1:-0}"
}

while [ $# -gt 0 ]; do
    case "$1" in
        --base) base="${2:?}"; shift 2 ;;
        --repo) repo="${2:?}"; shift 2 ;;
        --missing) missing="${2:?}"; shift 2 ;;
        --cold) cold="${2:?}"; shift 2 ;;
        --key) key="${2:?}"; shift 2 ;;
        --requests) requests="${2:?}"; shift 2 ;;
        --concurrency) concurrency="${2:?}"; shift 2 ;;
        -h|--help) usage 0 ;;
        *) echo "未知参数: $1" >&2; usage 1 ;;
    esac
done

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

# request URL [extra curl args...] → "耗时秒 状态码 响应字节"
request() {
    local url="$1"
    shift
    curl -sS -o /dev/null --max-time "$timeout" -w '%{time_total} %{http_code} %{size_download}' "$@" "$url"
}

# 从已排序的耗时文件里取分位（秒，保留 3 位）
percentile() {
    local file="$1" quantile="$2"
    awk -v q="$quantile" '
        { value[NR] = $1 }
        END {
            if (NR == 0) { printf "n/a"; exit }
            idx = int((NR - 1) * q + 0.5) + 1
            if (idx < 1) { idx = 1 }
            if (idx > NR) { idx = NR }
            printf "%.3f", value[idx]
        }
    ' "$file"
}

# 采集 N 次请求，输出 "p50 p95 max 平均体积 状态码分布"
measure() {
    local label="$1" url="$2"
    shift 2
    local times="$workdir/${label}.times" codes="$workdir/${label}.codes" sizes="$workdir/${label}.sizes"
    : > "$times"; : > "$codes"; : > "$sizes"
    for _ in $(seq 1 "$requests"); do
        # 加随机 query 参数绕开 nginx / 浏览器 / CDN 缓存，确保打到源站；
        # 服务端忽略未知参数，theme/locale 仍是合法值。
        local probe="$url&bench=$(od -An -N4 -tu4 < /dev/urandom | tr -d ' ')"
        local out
        out="$(request "$probe" "$@")"
        echo "${out%% *}" >> "$times"
        echo "$out" | awk '{print $2}' >> "$codes"
        echo "$out" | awk '{print $3}' >> "$sizes"
    done
    sort -n -o "$times" "$times"
    local p50 p95 max_size avg_size code_summary
    p50="$(percentile "$times" 0.50)"
    p95="$(percentile "$times" 0.95)"
    max_size="$(sort -n "$sizes" | tail -1)"
    avg_size="$(awk '{total += $1} END {printf "%.0f", total / NR}' "$sizes")"
    code_summary="$(sort "$codes" | uniq -c | awk '{printf "%s×%s ", $2, $1}')"
    printf '%-11s p50=%-7s p95=%-7s 体积 avg=%-7s max=%-8s 状态 %s\n' \
        "$label" "${p50}s" "${p95}s" "$avg_size" "$max_size" "$code_summary"
}

metrics_available() { [ -n "$key" ]; }

metrics_snapshot() {
    curl -sS --max-time "$timeout" -H "Authorization: Bearer $key" "$base/internal/metrics/service"
}

metrics_reset() {
    curl -sS --max-time "$timeout" -X POST -H "Authorization: Bearer $key" \
        "$base/internal/metrics/service/reset" > /dev/null
}

# 从快照 JSON 里取一个计数字段
metric_field() {
    local json="$1" field="$2"
    echo "$json" | sed -n "s/.*\"$field\":\([0-9]*\).*/\1/p" | head -1
}

echo "==================================================================="
echo "starcat-history-api 压测   目标: $base"
echo "仓库: $repo   每场景请求数: $requests   并发: $concurrency"
echo "计数差值: $(metrics_available && echo '启用' || echo '未启用（--key 或 STARCAT_API_KEY，仅测延迟）')"
echo "==================================================================="

echo
echo "--- 场景 1/4: 热路径（SVG / 曲线 JSON） ---"
measure "warm-svg" "$base/embed/v1/repos/$repo/star-history.svg?theme=light&locale=en"

# 曲线接口的缓存键是 repo_id；先取一次拿到 id，再测带 id 的快路径，
# 这样脚本能自己区分「带 id 的快」和「不带 id 的默认用法」。
curve_body="$workdir/curve.json"
curl -sS --max-time "$timeout" -o "$curve_body" "$base/api/v1/repos/$repo/star-history?range=1y" || true
repo_id="$(sed -n 's/.*"repo_id":\([0-9]*\).*/\1/p' "$curve_body" | head -1)"
if [ -n "$repo_id" ]; then
    measure "warm-json" "$base/api/v1/repos/$repo/star-history?repo_id=$repo_id&range=1y"
else
    echo "warm-json  跳过：未能解析 repo_id"
fi

echo
echo "--- 场景 2/4: 第三方默认用法（曲线 JSON 不带 repo_id）---"
if metrics_available; then
    metrics_reset
fi
measure "json-no-id" "$base/api/v1/repos/$repo/star-history?range=1y"
if metrics_available; then
    snapshot="$(metrics_snapshot)"
    echo "            GitHub metadata 回源: $(metric_field "$snapshot" github_metadata_requests) 次 / $requests 请求"
    echo "            （期望：远小于请求数。等于请求数说明缓存没被读到）"
fi

echo
echo "--- 场景 3/4: 不存在仓库的重复抓取（负缓存）---"
if metrics_available; then
    metrics_reset
fi
measure "404-flood" "$base/embed/v1/repos/$missing/star-history.svg?theme=light&locale=en"
if metrics_available; then
    snapshot="$(metrics_snapshot)"
    echo "            GitHub metadata 回源: $(metric_field "$snapshot" github_metadata_requests) 次 / $requests 请求"
    echo "            （期望：≤1。等于请求数说明每次都在打 GitHub）"
fi

echo
echo "--- 场景 4/4: 同仓库并发冷启动（请求合并）---"
if [ -z "$cold" ]; then
    echo "cold-burst  跳过：加 --cold owner/repo 指定一个尚未缓存的仓库"
else
    if metrics_available; then
        metrics_reset
    fi
    start_ns="$(date +%s%N 2>/dev/null || python3 -c 'import time;print(int(time.time()*1e9))')"
    # 用序号而不是 $! 命名输出文件：重定向在命令被放入后台之前就求值，
    # 此时 $! 还是上一次的值（首次甚至未定义），set -u 下会直接报错。
    for i in $(seq 1 "$concurrency"); do
        request "$base/embed/v1/repos/$cold/star-history.svg?theme=light&locale=en&burst=$i" \
            > "$workdir/cold.$i.out" &
    done
    wait
    end_ns="$(date +%s%N 2>/dev/null || python3 -c 'import time;print(int(time.time()*1e9))')"
    elapsed_ms=$(( (end_ns - start_ns) / 1000000 ))
    echo "cold-burst  并发 $concurrency 个请求整体耗时 ${elapsed_ms}ms"
    sort "$workdir"/cold.*.out | awk '{printf "            状态 %s 耗时 %ss\n", $2, $1}'
    if metrics_available; then
        snapshot="$(metrics_snapshot)"
        echo "            GitHub metadata 回源: $(metric_field "$snapshot" github_metadata_requests) 次"
        echo "            GitHub history  回源: $(metric_field "$snapshot" github_history_requests) 次"
        echo "            （期望：metadata = 1，history = 冷启动分页次数，不随并发放大）"
    fi
fi

echo
echo "==================================================================="
echo "判定口径："
echo "  - warm-svg p50 在服务端本机应 ≤ 5ms（公网测量被 RTT 支配，看相对值）"
echo "  - json-no-id 的回源次数必须远小于请求数"
echo "  - 404-flood 的回源次数必须 ≤ 1"
echo "  - cold-burst 的 metadata 回源必须恰好 1"
echo "==================================================================="
