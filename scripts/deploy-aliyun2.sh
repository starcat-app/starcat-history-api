#!/bin/bash
# ============================================================================
# starcat-history-api aliyun2 公开实例部署脚本
#
# 用法:
#   ./deploy-aliyun2.sh          交叉编译、上传二进制、更新 systemd 服务与 nginx 配置
#   DEPLOY_SSH_KEY=~/.ssh/server ./deploy-aliyun2.sh
#                                使用指定私钥连接远程服务器，避免本机 ssh alias 绑定到错误 key
#
# 部署形态:
#   - 服务器无 Go / Docker；本地 CGO_ENABLED=0 交叉编译静态二进制后 rsync 上传
#   - systemd 管理（Restart=always + multi-user.target），崩溃自动拉起、开机自启
#   - nginx 反代 127.0.0.1:5014，公开入口 https://history.starcat.ink
#
# 前置条件:
#   - ~/.ssh/config 中已配置 aliyun2 别名
#   - 远程 /etc/nginx/encrypt/starcat/ 已有 *.starcat.ink 通配符证书
# ============================================================================

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
REMOTE_HOST="aliyun2"
REMOTE_DIR="/opt/starcat/history-api"
SERVICE_NAME="starcat-history-api"
NGINX_CONF_LOCAL="$REPO_ROOT/scripts/history.starcat.ink.conf"
NGINX_CONF_REMOTE="/etc/nginx/conf.d/history.starcat.ink.conf"
LISTEN_PORT="5014"
DEPLOY_SSH_KEY="${DEPLOY_SSH_KEY:-}"

SSH_CMD=(ssh)
RSYNC_SSH="ssh"
if [ -n "$DEPLOY_SSH_KEY" ]; then
    SSH_CMD=(ssh -i "$DEPLOY_SSH_KEY")
    RSYNC_SSH="ssh -i $DEPLOY_SSH_KEY"
fi

echo "================================"
echo "部署 starcat-history-api 公开实例"
echo "本地仓库:  $REPO_ROOT"
echo "远程服务器: $REMOTE_HOST"
echo "远程目录:  $REMOTE_DIR"
echo "公开入口:  https://history.starcat.ink"
echo "================================"

if [[ "$(uname -s)" != "Darwin" ]] && ! command -v go >/dev/null 2>&1; then
    echo "错误: 本机未找到 Go 工具链，无法交叉编译。" >&2
    exit 1
fi
if [ ! -f "$NGINX_CONF_LOCAL" ]; then
    echo "错误: 找不到 Nginx 配置文件: $NGINX_CONF_LOCAL" >&2
    exit 1
fi

echo "交叉编译 linux/amd64 静态二进制..."
mkdir -p "$REPO_ROOT/bin"
(
    cd "$REPO_ROOT"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
        go build -trimpath -ldflags "-s -w" \
        -o bin/starcat-history-api-linux-amd64 ./cmd/server
)
ls -lh "$REPO_ROOT/bin/starcat-history-api-linux-amd64"

echo "上传二进制..."
"${SSH_CMD[@]}" "$REMOTE_HOST" "mkdir -p '$REMOTE_DIR/bin' '$REMOTE_DIR/data'"
rsync -avz --progress \
    -e "$RSYNC_SSH" \
    "$REPO_ROOT/bin/starcat-history-api-linux-amd64" \
    "$REMOTE_HOST:$REMOTE_DIR/bin/starcat-history-api.new"
# 先落临时名再原子替换，避免覆盖正在运行的二进制导致 "text file busy"。
"${SSH_CMD[@]}" "$REMOTE_HOST" \
    "chmod 0755 '$REMOTE_DIR/bin/starcat-history-api.new' && mv -f '$REMOTE_DIR/bin/starcat-history-api.new' '$REMOTE_DIR/bin/starcat-history-api'"

echo "同步 .env 配置..."
# 优先上传本机 .env（含 GITHUB_TOKENS 等），但本机相对路径在服务器上无效：
# 路径类 key 统一改写为远程绝对路径，其余配置原样保留。
# 本机没有 .env 时才退回远程生成（随机 API_KEYS + 空 token）。
ENV_LOCAL="$REPO_ROOT/.env"
if [ -f "$ENV_LOCAL" ]; then
    tmp_env="$(mktemp)"
    grep -v -E '^(PORT|STORE_FILE|REGISTRY_DIR|METRICS_STORE_FILE|PUBLISH_KEYS|MAX_BUNDLE_BYTES)=' "$ENV_LOCAL" > "$tmp_env"
    # 本机开发默认 key 不能上公网：复用远程已有 API_KEYS，远程没有则生成强随机值。
    if grep -q '^API_KEYS=local-history-client-key$' "$tmp_env"; then
        existing_key="$("${SSH_CMD[@]}" "$REMOTE_HOST" \
            "grep -E '^API_KEYS=.+' '$REMOTE_DIR/.env' 2>/dev/null | head -1 | cut -d= -f2-")"
        if [ -z "$existing_key" ]; then
            existing_key="$(openssl rand -hex 24)"
        fi
        sed -i '' "s/^API_KEYS=local-history-client-key$/API_KEYS=$existing_key/" "$tmp_env"
        echo "（本机为开发默认 API_KEYS，服务器沿用已有强 key）"
    fi
    {
        echo "PORT=$LISTEN_PORT"
        echo "STORE_FILE=$REMOTE_DIR/data/history.sqlite"
        echo "REGISTRY_DIR=$REMOTE_DIR/data/history-registry"
        echo "METRICS_STORE_FILE=$REMOTE_DIR/data/history-metrics.db"
    } >> "$tmp_env"
    rsync -avz --progress -e "$RSYNC_SSH" "$tmp_env" "$REMOTE_HOST:$REMOTE_DIR/.env.new" >/dev/null
    rm -f "$tmp_env"
    "${SSH_CMD[@]}" "$REMOTE_HOST" \
        "mv -f '$REMOTE_DIR/.env.new' '$REMOTE_DIR/.env' && chmod 0600 '$REMOTE_DIR/.env'"
    echo "✓ 已上传本机 .env（路径已改写为服务器值）"
else
    "${SSH_CMD[@]}" "$REMOTE_HOST" bash -s -- "$REMOTE_DIR" "$LISTEN_PORT" <<'REMOTE_ENV_SETUP'
set -euo pipefail
remote_dir="$1"
listen_port="$2"
env_file="$remote_dir/.env"
if [ -f "$env_file" ]; then
    echo ".env 已存在，跳过生成。"
    exit 0
fi
api_key="$(openssl rand -hex 24)"
cat > "$env_file" <<EOF
PORT=$listen_port
# 第三方曲线 / SVG 入口已公开；API_KEYS 仅保护 /api/v1/ping 与 /internal/*。
API_KEYS=$api_key
STORE_FILE=$remote_dir/data/history.sqlite
REGISTRY_DIR=$remote_dir/data/history-registry
METRICS_STORE_FILE=$remote_dir/data/history-metrics.db
METADATA_TTL_SECONDS=21600
OFFICIAL_MEMORY_CACHE_TTL_SECONDS=1800
MAXIMUM_HISTORY_POINTS=400
# 支持 GITHUB_TOKENS（逗号分隔多 token 轮询）或 GITHUB_TOKEN（单值）。
GITHUB_TOKENS=
GITHUB_API_ENDPOINT=https://api.github.com
EOF
chmod 0600 "$env_file"
echo "已生成 $env_file，客户端 API key: $api_key"
REMOTE_ENV_SETUP
fi

echo "安装 systemd 单元（Restart=always + 开机自启）..."
"${SSH_CMD[@]}" "$REMOTE_HOST" bash -s -- "$REMOTE_DIR" "$SERVICE_NAME" <<'REMOTE_UNIT_SETUP'
set -euo pipefail
remote_dir="$1"
service_name="$2"
unit_file="/etc/systemd/system/${service_name}.service"
cat > "$unit_file" <<EOF
[Unit]
Description=Starcat History API (public star history and SVG service)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$remote_dir
EnvironmentFile=$remote_dir/.env
ExecStart=$remote_dir/bin/starcat-history-api
Restart=always
RestartSec=3
# 服务只需写自身数据目录；/etc /usr 收敛为只读。
NoNewPrivileges=true
ProtectSystem=full
ReadWritePaths=$remote_dir

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable "$service_name" >/dev/null
systemctl restart "$service_name"
REMOTE_UNIT_SETUP

echo "健康检查（远程回环）..."
for _ in 1 2 3 4 5; do
    if "${SSH_CMD[@]}" "$REMOTE_HOST" "curl -fsS http://127.0.0.1:$LISTEN_PORT/healthz" >/dev/null 2>&1; then
        echo "✓ 服务健康"
        break
    fi
    sleep 2
done
if ! "${SSH_CMD[@]}" "$REMOTE_HOST" "curl -fsS http://127.0.0.1:$LISTEN_PORT/healthz" >/dev/null 2>&1; then
    echo "错误: 服务健康检查失败，查看日志: ssh $REMOTE_HOST journalctl -u $SERVICE_NAME -n 50" >&2
    exit 1
fi

echo "上传 Nginx 配置并重载..."
rsync -avz --progress \
    -e "$RSYNC_SSH" \
    "$NGINX_CONF_LOCAL" \
    "$REMOTE_HOST:/etc/nginx/conf.d/history.starcat.ink.conf"
if ! "${SSH_CMD[@]}" "$REMOTE_HOST" "nginx -t && systemctl reload nginx"; then
    echo "错误: Nginx 重载失败" >&2
    exit 1
fi

echo "公网端到端验证..."
curl -fsS "https://history.starcat.ink/healthz" >/dev/null \
    && echo "✓ https://history.starcat.ink/healthz OK" \
    || echo "⚠ 域名验证失败：请确认 history.starcat.ink DNS 已生效"

rm -f "$REPO_ROOT/bin/starcat-history-api-linux-amd64"
echo "================================"
echo "✓ 部署完成"
echo "服务状态: ssh $REMOTE_HOST systemctl status $SERVICE_NAME"
echo "服务日志: ssh $REMOTE_HOST journalctl -u $SERVICE_NAME -f"
echo "================================"
