# starcat-history-api

Starcat 的公开 GitHub 仓库 Star 历史数据管道与后端 API。项目包含两部分：

- 本地 Builder：读取 GH Archive WatchEvent Parquet，生成日级 Silver、完整 Snapshot 和每日 Delta。
- 云端 API：接收经过校验的 Snapshot/Delta，通过 GitHub 当前 `stargazers_count` 校准历史曲线并向 Starcat 或第三方提供 REST API。

原始 WatchEvent 始终保留在本地数据盘；云端只保存 `repo_id + 日事件数` 的压缩序列，不保存用户身份、actor、payload 或私有仓库数据。

## 数据流

```text
GH Archive WatchEvent Raw Parquet
  -> History Silver (repo_id + event_day + event_count)
  -> Snapshot / Daily Delta ZIP
  -> starcat-history-api Registry
  -> GET /api/v1/repos/{owner}/{repo}/star-history
  -> Starcat 项目洞察
```

WatchEvent 不可靠表达 Unstar，因此服务返回的是估算曲线：

```text
estimatedStars(day) = round(currentStars * cumulativeEvents(day) / totalEvents)
```

所有公共点固定标记为 `source=gh_archive`、`precision=estimated`，最后一个覆盖点等于 GitHub 当前 Star 数。

## 环境要求

- Go 1.25+
- Python 3.11+ 与 [uv](https://docs.astral.sh/uv/)
- Builder 临时目录必须位于有足够空间的数据盘；全量任务不要使用系统盘默认临时目录

## 运行测试

```bash
make test
```

分别执行：

```bash
go test ./...
go vet ./...
cd builder
uv sync --extra test --python 3.12
uv run pytest -q
```

## 本地启动 API

```bash
cp .env.example .env
# 编辑 .env，至少设置 API_KEYS 和 PUBLISH_KEYS
make run
```

默认监听 `http://127.0.0.1:5014`。

```bash
curl -fsS http://127.0.0.1:5014/healthz

curl -fsS \
  -H 'Authorization: Bearer local-history-client-key' \
  http://127.0.0.1:5014/api/v1/ping
```

## 构建 History Silver

下面示例直接读取已经下载的 Raw WatchEvent 分区：

```bash
cd builder

uv run starcat-history-builder silver \
  --input '/Volumes/T0/Starcat/bigquery/watch-events-2016-2026/raw/gh_archive/watch-events-*.parquet' \
  --output-dir /Volumes/T0/Starcat/history/silver \
  --temp-dir /Volumes/T0/Starcat/history/tmp \
  --dataset-id watch-silver-2016-20260825-v1 \
  --watermark 2026-08-25 \
  --memory-limit 12GB \
  --threads 6
```

Silver 按 `event_year` 分区，manifest 记录输入文件数、仓库数、repo-day 数、WatchEvent 总数以及每个 Parquet 文件的 SHA-256。

## 构建 Snapshot

Snapshot 可以读取 Raw、Trainer Canonical 或 History Silver。生产建议从 Silver 重建：

```bash
cd builder

uv run starcat-history-builder snapshot \
  --input '/Volumes/T0/Starcat/history/silver/watch-silver-2016-20260825-v1/data/**/*.parquet' \
  --output-dir /Volumes/T0/Starcat/history/snapshots \
  --temp-dir /Volumes/T0/Starcat/history/tmp \
  --model-version watch-history-20260825-v1 \
  --watermark 2026-08-25 \
  --memory-limit 12GB \
  --threads 6
```

联调时可重复传 `--repo-id` 构建定向小快照。全量生产构建不要传该参数。

产物目录包含：

```text
watch-history-20260825-v1/
├── history.sqlite
├── manifest.json
├── checksums.json
└── watch-history-20260825-v1.zip
```

Snapshot manifest v2 会记录 Builder 已完成的 `PRAGMA quick_check` 结果和 SQLite
字节数。发布端仍校验鉴权、ZIP 白名单、流式 SHA-256、必要表和激活水位，但不会再次
完整扫描大数据库；上传后立即激活也会复用同一次校验结果。旧的 manifest v1 仍可
用于已安装快照的恢复和回滚。

## 本地安装 Snapshot（推荐联调）

本机只跑查询时，**只需 `history.sqlite`**，不必拷贝 `manifest.json` / `checksums.json` / `*.zip`，也不必拷 Silver / Raw Parquet。

先停掉占用 `5014` 的 history-api，再执行：

```bash
# 停服务后再装，避免半截 WAL
make install-local-snapshot \
  SNAPSHOT=/Volumes/T0/Starcat/history/snapshots/watch-history-20260825-v1

# 目标已存在时覆盖
make install-local-snapshot \
  SNAPSHOT=/Volumes/T0/Starcat/history/snapshots/watch-history-20260825-v1 \
  FORCE=1
```

等价脚本：

```bash
scripts/install-local-snapshot.sh \
  /Volumes/T0/Starcat/history/snapshots/watch-history-20260825-v1

# 也可直接传 sqlite 路径
scripts/install-local-snapshot.sh \
  /Volumes/T0/Starcat/history/snapshots/watch-history-20260825-v1/history.sqlite \
  --force
```

默认写入 `.env` 的 `STORE_FILE`（未配置则 `./data/history.sqlite`）。脚本会：

1. 拒绝在端口仍监听时覆盖（除非 `--allow-running`）
2. 校验源库含 `repo_history_series` / `history_active`
3. 原子替换目标库，并清理旧的 `-wal` / `-shm`
4. 打印 active 水位与仓库数

然后启动：

```bash
make run
curl -fsS http://127.0.0.1:5014/healthz
curl -fsS -H 'Authorization: Bearer local-history-client-key' \
  http://127.0.0.1:5014/internal/stats | jq .
```

说明：

- 这条路径走 **bootstrap `STORE_FILE`**，`data/history-registry` 可以为空。
- 正式环境或需要日增量时，仍用下面的 `publish-bundle.sh`（ZIP → Registry 激活）。
- `history-registry/`、Silver、Raw 数据**不需要**为本地查询再拷一份。

## 构建每日 Delta

Delta 是 `(from_watermark, to_watermark]` 的相邻 UTC 日增量，服务端会拒绝日期缺口：

```bash
cd builder

uv run starcat-history-builder delta \
  --input /Volumes/T0/Starcat/bigquery/watch-events-2016-2026/raw/gh_archive/watch-events-20260826.parquet \
  --output-dir /Volumes/T0/Starcat/history/deltas \
  --temp-dir /Volumes/T0/Starcat/history/tmp \
  --delta-id watch-delta-20260826-v1 \
  --from-watermark 2026-08-25 \
  --to-watermark 2026-08-26 \
  --memory-limit 4GB \
  --threads 4
```

生产日常任务使用完整闭环脚本。它不会访问 BigQuery，只消费本地数据平台已经下载完成的
单日 Raw 分区；随后依次校验服务端水位、构建不可变 Silver、生成相邻 Delta、流式上传并
写入 `publish-receipt.json`。服务端水位已到达目标日期时会直接返回成功，因此任务可安全重跑：

```bash
export HISTORY_PUBLISH_KEY=local-history-publish-key
export HISTORY_BASE_URL=http://127.0.0.1:5014

scripts/run-daily-pipeline.sh 2026-08-26
```

默认目录：

- Raw：`/Volumes/T0/Starcat/bigquery/watch-events-2016-2026/raw/gh_archive`
- Silver：`/Volumes/T0/Starcat/history/silver/daily`
- Delta：`/Volumes/T0/Starcat/history/deltas`
- DuckDB spill：`/Volumes/T0/Starcat/history/spill`

可分别通过 `HISTORY_RAW_ROOT`、`HISTORY_DATA_ROOT`、`HISTORY_MEMORY_LIMIT` 和
`HISTORY_THREADS` 覆盖。通过聚合服务发布时设置
`HISTORY_BASE_URL=https://starcat-api.fly.dev` 与 `HISTORY_GATEWAY_SERVICE=history`；独立服务
不设置 gateway service。
Raw 文件必须只包含目标 UTC 日期；已有 Silver/Delta 的来源摘要不一致时任务会拒绝覆盖，
需要人工确认错误产物，而不是静默复用。

## 发布 Snapshot 与 Delta

独立服务不需要网关头：

```bash
export HISTORY_API_BASE_URL=http://127.0.0.1:5014
export HISTORY_PUBLISH_KEY=local-history-publish-key

scripts/publish-bundle.sh snapshot \
  watch-history-20260825-v1 \
  /Volumes/T0/Starcat/history/snapshots/watch-history-20260825-v1/watch-history-20260825-v1.zip \
  true

scripts/publish-bundle.sh delta \
  watch-delta-20260826-v1 \
  /Volumes/T0/Starcat/history/deltas/watch-delta-20260826-v1/watch-delta-20260826-v1.zip
```

通过聚合 `starcat-api` 发布时再设置：

```bash
export HISTORY_API_BASE_URL=http://127.0.0.1:8080
export HISTORY_GATEWAY_SERVICE=history
```

同一 Snapshot/Delta ID 和相同 checksum 可安全重放；同 ID 不同内容返回 `409`。
激活新 Snapshot 后默认保留最近 3 个版本（最少 2 个，确保可回滚），并删除水位已被
新 Snapshot 覆盖的 Delta 产物；可通过 `SNAPSHOT_RETENTION` 调整保留数。
服务端还会根据上传包 `Content-Length` 预留解压、可写 runtime 副本和 1 GiB 安全余量；
容量不足时在读取大包前返回 `507 INSUFFICIENT_STORAGE`。

## 查询 Star 历史

第三方兼容接口（服务端校准曲线；可选传入本地已知星标数以避免打 GitHub）：

```bash
curl -fsS \
  -H 'Authorization: Bearer local-history-client-key' \
  -H 'X-SC-Svc: history' \
  'http://127.0.0.1:5014/api/v1/repos/vinta/awesome-python/star-history?repo_id=21289110&range=all&current_stars=120000' \
  | jq .
```

- `current_stars` 可选。合法非负整数时直接 `Normalize`，不访问 GitHub。
- 未传时回退 GitHub metadata（含本地 metadata 缓存）。
- 支持 `range=3m|1y|all`、`ETag` / `If-None-Match`。Private/Internal 在走 GitHub 路径时仍会拒绝。

Starcat 专用原始事件接口（不访问 GitHub，由客户端用本地 `stars_count` 校准）：

```bash
curl -fsS \
  -H 'Authorization: Bearer local-history-client-key' \
  -H 'X-SC-Svc: history' \
  'http://127.0.0.1:5014/api/v1/repos/vinta/awesome-python/star-history/events?repo_id=21289110' \
  | jq .
```

`events[].count` 是当日 WatchEvent 数，不是累计星标。

## 接口

| 方法 | 路径 | 鉴权 | 用途 |
|---|---|---|---|
| GET | `/healthz` | 无 | 进程健康检查 |
| GET | `/api/v1/ping` | `API_KEYS` | 客户端连接检查 |
| GET | `/api/v1/repos/{owner}/{repo}/star-history` | `API_KEYS` | 查询公开仓库校准曲线（第三方） |
| GET | `/api/v1/repos/{owner}/{repo}/star-history/events` | `API_KEYS` | 查询原始日事件（Starcat） |
| GET | `/internal/stats` | `API_KEYS` | 常量时间读取 Serving 规模与水位 |
| GET | `/internal/metrics/*` | `API_KEYS` | 调用统计 |
| POST | `/internal/v1/history-snapshots/{version}?activate=true` | `PUBLISH_KEYS` | 安装/激活快照 |
| POST | `/internal/v1/history-snapshots/{version}/activate` | `PUBLISH_KEYS` | 回切已安装快照 |
| POST | `/internal/v1/history-deltas/{delta_id}` | `PUBLISH_KEYS` | 幂等应用日增量 |
| GET | `/internal/v1/history-active` | `PUBLISH_KEYS` | 当前版本和水位 |

## 安全边界

- 公共查询 Key 与内部发布 Key 必须分离。
- ZIP 只接受规定文件白名单，并校验流式 SHA-256、manifest、SQLite schema 和激活水位。
- Snapshot 的完整 `PRAGMA quick_check` 由正式 Builder 执行一次并写入 manifest v2；较小的 Delta 仍由发布端执行完整校验。
- Snapshot 激活使用版本目录和原子 active pointer；失败不会切换当前查询版本。
- 仓库数、repo-day 和 WatchEvent 总量由 Snapshot 固化、Delta 事务内递增；统计接口不会扫描全量序列表或占用查询连接。
- 服务不连接 BigQuery，也不读取家庭数据盘；它只消费本地平台主动发布的 Serving 产物。
- GitHub Token 只用于读取公开仓库当前 metadata；未配置时受 GitHub 匿名限额约束。

## 聚合部署

生产环境作为 `starcat-api` 的第七个模块运行，通过 `X-SC-Svc: history` 分流；无需新增独立 Fly App。独立二进制与 Dockerfile仍保留，方便本地验证和第三方自托管。

## License

[MIT](./LICENSE)
