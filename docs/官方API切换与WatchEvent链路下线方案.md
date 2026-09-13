# 官方 API 切换与 WatchEvent 链路下线方案

- 日期：2026-09-13
- 状态：方案待评审（未动手，本文先行）
- 范围：`starcat-history-api` 本体为主，涉及 `starcat-api`（聚合部署）、主仓库 Starcat（App + supports/scripts）、`starcat-recsys-trainer` 的配套清理

---

## 1. 背景

### 1.1 本服务最初的数据来源

`starcat-history-api` 最初是围绕 GH Archive WatchEvent 搭建的本地星标历史服务：

```text
BigQuery GH Archive WatchEvent 日表
  -> starcat-recsys-trainer 下载十年 Raw Parquet（/Volumes/T0 移动硬盘）
  -> builder/（Python）：Raw -> Silver 日分区 -> Delta/Snapshot ZIP
  -> POST /internal/v1/history-snapshots|history-deltas（PUBLISH_KEYS 鉴权）发布到生产
  -> serving.Registry 的 repo-day series（repo_history_series）
  -> GET /api/v1/repos/{owner}/{repo}/star-history/events（Starcat 客户端专用原始事件端点）
```

这条链路的日常运维由主仓库的 `supports/scripts/run-history-daily-sync.sh`（launchd 定时触发）串联，
依赖一整套本机基础设施：`gcloud`/BigQuery 认证、T0 移动硬盘挂载、macOS 完全磁盘访问、
Keychain 里的 `HISTORY_PUBLISH_KEY`、Builder Python 虚拟环境（515 个 py 文件 / 57MB）。

### 1.2 GitHub 提供官方 Star 历史 API 后，链路失去存在意义

GitHub 现已提供官方的仓库星标历史 API（按周返回 `week` / `total` / 七日增量）。
服务端与 App 已经先后切换到官方数据源，切换工作**均已完成并发布**：

| 侧 | 切换点 | 状态 |
|------|----------|------|
| 服务端 | `handler.NewHistoryHandler` 无条件装配 `WithStarHistoryProvider`，公开曲线与 SVG Embed 均走官方 API + SQLite 周缓存(24h) + 内存 LRU | 已上线，主路径 |
| App | `b496533d`（2026-09-08）「星标历史切换为 GitHub 官方唯一数据源」，随 v1.6.1（2026-09-09 tag）发布；`RepoStarHistoryRepository` 只注入 `GitHubStarHistoryAPIProtocol` | 已发布 |

当前真实存在的数据流只剩一条：

```text
GitHub /repos/{owner}/{repo}/stargazers/history（官方）
  -> SQLite 周缓存 + 内存 LRU
  -> GET /api/v1/repos/{owner}/{repo}/star-history（曲线）
  -> GET /embed/v1/repos/{owner}/{repo}/star-history.svg（README 外嵌 SVG）
```

WatchEvent 链路的产物目前只有两个消费者，都是可拆除的残留：

1. `GET /star-history/events` 端点——dev 分支上 App 已无任何调用方
   （`StarHistoryAPI` 在 `AppDependencies.swift:1355` 实例化后只被设置页更新 URL/key，
   不再用于取数，属死代码）；但 **v1.6.0 及更早的已发布客户端仍在调用**。
2. embed / curve handler 里 `starHistory == nil` 时的 fallback 分支——生产配置下不可达。

README 已如实标注现状：`Builder, Snapshot, and Delta remain as a separate legacy raw-event
data path and are not used to fetch public curve history.`（"remain" 即本次要收口的债务。）

---

## 2. 为什么下线（收益）

1. **消除唯一的重运维依赖**。官方路径是无状态拉取 + 缓存；WatchEvent 链路则要求一台
   常开 mac（launchd + T0 硬盘 + BigQuery 配额门禁 + 发布密钥轮换）。停掉它，
   服务的日常运维成本归零，不再有「硬盘没挂载 → 今天增量没发」这类故障面。
2. **数据质量不再服务两个主人**。WatchEvent 反推的 repo-day 序列是
   `source=gh_archive / precision=estimated` 的估算数据；官方 API + 当前 Star 数校准
   （`source=github_history / precision=reconstructed`）是唯一事实源。两套精度并存
   会让外部使用者困惑，收敛后语义单一。
3. **自托管门槛大幅下降**。下线后自托管 `starcat-history-api` 只需要 Go + 一个
   `GITHUB_TOKEN`，不再需要 Python/uv、BigQuery、十年级磁盘空间。
   本服务的定位正是「对第三方提供通过项目名称生成 SVG 的公开服务」，这是核心收益。
4. **仓库瘦身**。`builder/`（57MB 源码 + 独立测试体系 + 4 个运维脚本）整体删除，
   Go 测试与 CI 面同步收窄。
5. **聚合部署减压**。`starcat-api` 单 Machine（shared-cpu-1x / 1GB）上，
   snapshot 上传的 600s idle_timeout、`MAX_BUNDLE_BYTES` 流式解压校验等
   为 Builder 特化的配置可以一并退场。

## 3. 目标形态

```text
starcat-history-api = 「仓库名 -> SVG / 曲线」的公开只读服务

GitHub 官方 stargazers/history
  -> metadata 公开性校验（缓存）
  -> 周数据缓存（SQLite 24h + 内存 LRU，ETag 增量刷新）
  -> 日级累计曲线重建 + 当前 Star 数校准
  -> 曲线 JSON 端点 + README SVG Embed
```

保留：`/healthz`、`/api/v1/ping`、`/api/v1/repos/{owner}/{repo}/star-history`、
`/embed/v1/repos/{owner}/{repo}/star-history.svg`、`/internal/stats`、`/internal/metrics/*`。

移除：`/api/v1/repos/{owner}/{repo}/star-history/events`、
`POST /internal/v1/history-snapshots/{version}`(+activate)、
`POST /internal/v1/history-deltas/{delta_id}`、`GET /internal/v1/history-active`。

---

## 4. 分阶段改造方案

> 各阶段可独立收口、独立提交；阶段间只有顺序依赖（先停发布方，再拆消费方）。

### 阶段 1：history-api 服务端删 legacy（Go）

| 动作 | 涉及位置 |
|------|----------|
| 删 `/star-history/events` 路由与 handler | `internal/handler/history.go`（events 部分）、`server/server.go:154` |
| 删 4 个 `/internal/v1/history-*` 发布端点 + `publish.go` + publish 鉴权 | `server/server.go:163-166`、`internal/handler/publish.go`、`internal/middleware/auth.go`（publish 实例） |
| 删 embed/curve 中 `starHistory == nil` 的 fallback 分支，官方 provider 改为必需依赖 | `internal/handler/embed.go`、`history.go` |
| `serving.Registry` 瘦身：删 Series / Active / InstallSnapshotZip / InstallDeltaZip / ActivateSnapshot / 快照保留与 attestation；**保留** metadata 缓存、`GitHubStarHistoryCache`、OperationalStats | `internal/serving/registry.go`、`store.go` |
| 删 legacy 序列 codec 与模型 | `internal/series/history.go`（保留 `official.go`）、`internal/model/history.go` 中 legacy 类型 |
| 清理环境变量 | `PUBLISH_KEYS`、`MAX_BUNDLE_BYTES`、`SNAPSHOT_RETENTION`、（视瘦身结果）`REGISTRY_DIR` |
| 测试同步删改 | `embed_test.go`、`history_test.go`、`registry_test.go`、`store_test.go` 等 |

生产 `/data/history.db` 中的 legacy 表（repo-day series 等）**不做迁移清理**：
代码不再读取即可，避免对自托管实例做破坏性 schema 操作。

### 阶段 2：删除 builder/ 与运维脚本

- 删除 `builder/` 整个目录（Python 项目 + pytest 体系）。
- 删除 `scripts/run-daily-catch-up.sh`、`scripts/run-daily-pipeline.sh`、
  `scripts/publish-bundle.sh`、`scripts/install-local-snapshot.sh`。
- `README.md` / `README-ZH.md` 重写：Data flow 收敛为单条官方链路；
  Requirements 去掉 Python 3.11+/uv；删除 Silver/Snapshot/Delta/发布 全部章节；
  「首次建库」概念消亡（官方路径冷启动即自动回填）。
- **破坏性变更**：对自托管用户，发布端点与 `/events` 消失。版本号建议升 major，
  Changelog 说明迁移方式（无需迁移——官方路径零配置可用）。

### 阶段 3：starcat-api 聚合服务配套

- `fly.toml`：删 `HISTORY_SNAPSHOT_RETENTION`、snapshot 上传注释与
  `idle_timeout = 600`（删前确认无其他慢请求依赖该超时）、`HISTORY_REGISTRY_DIR`（视阶段 1 结果）。
- `fly secrets unset PUBLISH_KEYS`。
- `cmd/server/main.go` 挂载方式不变（history 仍是 7 服务之一）；
  顺手修正 README promo 文案「mounts six packages」→ 实际 7 个。

### 阶段 4：主仓库 Starcat 与 recsys-trainer 清理

主仓库：

- 删 `supports/scripts/run-history-daily-sync.sh`、`install-history-daily-launch-agent.sh`、
  `history-daily.env.example`，注销本机 launchd 定时任务。
- App 死代码清理：`StarHistoryAPI.swift`、`AppEndpoints.History.Paths.starHistoryEvents`、
  `AppDependencies.starHistoryAPI` 属性及 `updateBaseURL/updateAPIKey` 的 `.history` 分支。
  `StarHistoryAPIError` 仍被 repository/viewmodel 用作错误类型，保留（或另行改名）。
- `docs/2-产品/需求讨论/推荐算法/WatchEvent与Star-History每日增量运维指南.md`（421 行）
  大半内容讲 history 发布链路，重写为「WatchEvent 每日下载（仅推荐算法用）」或直接退役。

recsys-trainer（**下载与训练管道本身不动**，推荐算法仍依赖 WatchEvent 数据）：

- 仅清理文档中「数据交给 history-api」的交接描述：
  `docs/使用说明.md`（164-173 行附近）、`docs/架构设计.md`（40-52 行附近）。

### App 侧不动的部分（明确排除）

- `StarHistorySource.ghArchive` / `.discoverySnapshot` 枚举 case 保留：
  已发布版本的本地缓存行里存有这些 source 值，删除会破坏解码；只是新数据不再写入。
  不做任何 schema 变更（铁律 #1）。
- `ThirdPartyService` 设置页的 History 服务条目是否整体移除另议（见 §5 决策点 2）。

---

## 5. 风险与决策点（需 dong4j 拍板）

### 决策点 1：`/events` 端点的下线窗口 ⚠️ 关键

官方源切换随 **v1.6.1（2026-09-09）** 发布，距今仅数天。v1.6.0 及更早的线上客户端
仍在调用 `/star-history/events`，且老版本 App 端可能没有 fallback——直接删端点
等于让这批用户的星标历史卡片空白。

可选策略：

- **A（推荐）**：阶段 1 先不删 `/events`，改为返回 `410 Gone`（或保留只读、停止更新），
  同时通过 `/internal/metrics/routes` 观察该路由日请求量；待 v1.6.1 渗透率足够
  （流量趋近于零）后二次提交物理删除。
- **B**：接受降级，随阶段 1 直接删除。收益是一步到位，代价是老客户端体验受损。

### 决策点 2：设置页 History 服务条目

App 已不消费 history-api 任何端点，`ThirdPartyService` 里的 History 条目
（URL / API Key 配置 + 连通性测试）成为摆设。移除涉及已发布用户的偏好存储
（是否需要偏好迁移、还是静默忽略），按铁律 #1 单独评估，建议与本次解耦、另行排期。

### 决策点 3：recsys-trainer 的每日 WatchEvent catch-up 是否保留

`run-history-daily-sync.sh` 前半段（trainer 下载每日 Raw）同时服务推荐算法训练。
若十年一次性训练已满足推荐需求，每日下载任务可一并停止；
若推荐仍需持续增量，则保留 trainer 侧下载、仅摘除 history 发布腿。

### 风险清单

| 风险 | 缓解 |
|------|------|
| 老客户端 `/events` 依赖（见决策点 1） | 410 过渡期 + 流量观察 |
| 生产历史数据「缩水」：WatchEvent 估算序列 vs 官方重建序列曲线形状略有差异 | 官方路径已上线数月，SVG 输出早已切换，无用户可感知变化 |
| 遗漏隐藏消费者（CLI / 插件 / 官网直接调 `/events` 或发布端点） | 下线前全 `supports/` + 官网仓库 grep 一遍端点路径 |
| 自托管用户依赖发布端点做私有化数据导入 | major 版本 + Changelog 明示；官方路径自托管零配置 |

---

## 6. 执行核对清单（动代码前逐项确认）

- [ ] `grep -rn "star-history/events"` 全 `supports/` 与官网仓库，确认无新消费者
- [ ] `/internal/metrics/routes` 拉取 `/events` 近 30 天流量，确定决策点 1 策略
- [ ] 确认聚合部署 `fly secrets` 中 `PUBLISH_KEYS` 当前值可安全移除
- [ ] 确认主仓库 launchd 已加载的 `run-history-daily-sync` 任务句柄（`launchctl` list）
- [ ] 总览登记：本次改造在 `docs/功能实现总览.md` 的条目文案，需 dong4j 确认后写入
