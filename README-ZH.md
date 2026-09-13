# starcat-history-api

<!-- starcat-promo:start -->
<div align="center">
<a href="https://starcat.ink"><img src="https://raw.githubusercontent.com/starcat-app/starcat-pro/main/banner.webp" width="100%" alt="Starcat" /></a>

<p><strong>Starcat 的可自部署 API 与缓存优先的公开 GitHub 星标历史服务。</strong></p>
<p>Starcat 是一款原生 macOS 应用，可以把 GitHub Stars 变成可搜索、可整理、可用 AI 追问的本地知识库，并通过桌面客户端、插件、CLI 与可自部署服务组成完整生态。</p>

<a href="https://github.com/starcat-app/homebrew-starcat"><img src="https://img.shields.io/badge/Install%20with-Homebrew-FBBF24?style=for-the-badge&logo=homebrew&logoColor=white" width="220" alt="Install with Homebrew"/></a>
<br/>
<sub><a href="./README.md">English</a></sub>
</div>

<div align="center">
<a href="https://starcat.ink"><img src="https://img.shields.io/badge/website-starcat.ink-38BDF8?style=flat&color=blue" alt="website"/></a>
<a href="https://github.com/starcat-app/starcat-pro"><img src="https://img.shields.io/badge/support-starcat--pro-lightgrey.svg?style=flat&color=blue" alt="support"/></a>
<a href="https://github.com/starcat-app/homebrew-starcat"><img src="https://img.shields.io/badge/install-homebrew-lightgrey.svg?style=flat&color=blue" alt="homebrew"/></a>
<a href="https://github.com/starcat-app/starcat-localization"><img src="https://img.shields.io/badge/localization-open-lightgrey.svg?style=flat&color=blue" alt="localization"/></a>
</div>

<div align="center">
<img width="900" src="https://raw.githubusercontent.com/starcat-app/starcat-pro/main/main.webp" alt="Starcat main window"/>
</div>

**首选 Homebrew 安装：**

```bash
brew tap starcat-app/starcat
brew trust starcat-app/starcat
brew install --cask starcat
```

**相关链接：**

- 官网与下载: https://starcat.ink
- Mac App Store: 搜索 Starcat for GitHub
- 公开支持与发布说明: https://github.com/starcat-app/starcat-pro
- CLI / MCP: [starcat-cli](https://github.com/starcat-app/starcat-cli) / [Homebrew tap](https://github.com/starcat-app/homebrew-starcat-cli)
- AI Agent Skill: https://github.com/starcat-app/starcat-skill
- 浏览器插件: [Chrome](https://github.com/starcat-app/starcat-chrome-plugin) / [Safari](https://github.com/starcat-app/starcat-safari-plugin)
- 官方文档: https://github.com/starcat-app/starcat-docs
- 官网源码: https://github.com/starcat-app/starcat-site
- 本地化: https://github.com/starcat-app/starcat-localization

**可自部署支撑 API：**

- [starcat-sharing-api](https://github.com/starcat-app/starcat-sharing-api)
- [starcat-trending-api](https://github.com/starcat-app/starcat-trending-api)
- [starcat-weekly-api](https://github.com/starcat-app/starcat-weekly-api)
- [starcat-wiki-api](https://github.com/starcat-app/starcat-wiki-api)
- [starcat-recommend-api](https://github.com/starcat-app/starcat-recommend-api)
- [starcat-discovery-api](https://github.com/starcat-app/starcat-discovery-api)
- [starcat-history-api](https://github.com/starcat-app/starcat-history-api)

> Starcat 为普通用户提供默认托管服务。这个 API 的设计让进阶用户可以审查实现、本地运行，或在仓库公开发布审查完成后部署自己的实例。
<!-- starcat-promo:end -->

<sub><a href="./README.md">English</a></sub>

starcat-history-api 是面向公开 GitHub 仓库的 Star History 服务。它只做一件事：给定一个公开仓库名，返回该仓库的星标历史——既可以是免鉴权查询的 JSON 曲线接口，也可以是可直接嵌入公开 README 的自包含 SVG 卡片。

## 核心能力

### 1. 星标历史查询接口

`GET /api/v1/repos/{owner}/{repo}/star-history` 返回公开仓库的累计星标曲线。数据来自 GitHub 官方 `stargazers/history` API，重建为日级累计点并用当前公开 `stargazers_count` 校准末端。内存 LRU + SQLite 两级缓存配合 ETag 增量刷新，高频请求不会反复访问 GitHub。

### 2. 在公开 README 中嵌入星标历史

`GET /embed/v1/repos/{owner}/{repo}/star-history.svg` 把同样的历史渲染成自包含 SVG 卡片。无需 API key、不依赖 JavaScript、远程样式或远程图片，可直接用于 GitHub README 渲染，支持明暗主题与中英文语言。

## 弃用公告

旧版 GH Archive WatchEvent 数据链路即将退役，将在后续版本移除：

- `GET /api/v1/repos/{owner}/{repo}/star-history/events`——原始日 WatchEvent 事件接口；
- `POST /internal/v1/history-snapshots/*`、`POST /internal/v1/history-deltas/*`、`GET /internal/v1/history-active`——Builder 发布接口；
- Builder 本体、Silver/Snapshot/Delta 产物，以及本地 WatchEvent 每日增量编排。

公开曲线与 SVG Embed 的响应早已全部来自 GitHub 官方 API，不受影响。新接入方请勿再使用上述端点。背景、理由与完整下线方案见[官方API切换与WatchEvent链路下线方案](./docs/官方API切换与WatchEvent链路下线方案.md)。

## 数据流

```text
GitHub /repos/{owner}/{repo}/stargazers/history
  -> SQLite 周数据缓存 + 内存 LRU
  -> 日级累计曲线 + 当前 Stars 校准
  -> GET /api/v1/repos/{owner}/{repo}/star-history
  -> Starcat 项目洞察 / SVG Embed
```

GitHub 官方接口返回每周 `week`、`total` 和 7 个每日新增值。由于接口不提供历史 Unstar，服务使用当前公开 Star 数作为末端校准锚点：

```text
estimatedStars(day) = round(currentStars * cumulativeEvents(day) / totalEvents)
```

所有公共点固定标记为 `source=github_history`、`precision=reconstructed`。首次访问会分页拉取官方历史，SQLite 数据缓存 24 小时；过期后使用 ETag 增量刷新，每 7 天做一次全量校验。

## 环境要求

- Go 1.25+
- Docker，仅在验证或构建容器镜像时需要

`GITHUB_TOKENS`（逗号分隔多 token 轮换分摊限额）或 `GITHUB_TOKEN`（单值）可选；未配置时服务运行在 GitHub 匿名限额之下。

## 运行测试

```bash
make test
```

分别执行：

```bash
go test ./...
go vet ./...
```

## 本地启动 API

```bash
cp .env.example .env
# 编辑 .env，至少设置 API_KEYS
make run
```

默认监听 `http://127.0.0.1:5014`。

官方 Star 历史的进程内缓存默认保留 30 分钟，可通过 `.env` 中的
`OFFICIAL_MEMORY_CACHE_TTL_SECONDS` 调整；该配置只影响内存层，SQLite 官方历史缓存仍为 24 小时。

```bash
curl -fsS http://127.0.0.1:5014/healthz

curl -fsS \
  -H 'Authorization: Bearer local-history-client-key' \
  http://127.0.0.1:5014/api/v1/ping
```

不要把 `.env`、API key、GitHub Token 或生成的 SQLite 数据库提交进仓库。

## 接口

| 方法 | 路径 | 鉴权 | 用途 |
|---|---|---|---|
| GET | `/healthz` | 无 | 进程健康检查 |
| GET | `/api/v1/ping` | `API_KEYS` | 客户端连接检查 |
| GET | `/embed/v1/repos/{owner}/{repo}/star-history.svg` | 无 | README 可嵌入的公开自包含 SVG |
| GET | `/api/v1/repos/{owner}/{repo}/star-history` | 无 | 查询公开仓库校准曲线 |
| GET | `/internal/stats` | `API_KEYS` | Serving 规模与缓存统计 |
| GET | `/internal/metrics/*` | `API_KEYS` | 调用统计 |

查询校准曲线：

```bash
curl -fsS \
  'http://127.0.0.1:5014/api/v1/repos/vinta/awesome-python/star-history?repo_id=21289110&range=all&current_stars=120000'
```

曲线接口直接使用 GitHub 官方历史数据，支持 `range=3m|1y|all`、`ETag` / `If-None-Match`。`repo_id` 可省略；首次请求会全量分页并写入 SQLite，后续进程内优先命中内存 LRU，跨重启优先命中 SQLite，过期后使用 ETag 增量刷新。传入合法非负 `current_stars` 时可直接校准，不访问 GitHub metadata。

### 在公开 README 中嵌入星标历史

公开 SVG 接口不需要 API key。将下面的 HTML 复制到公开仓库的 README，并替换 `OWNER` 和 `REPO`：

```html
<picture data-starcat-star-history>
  <source
    media="(prefers-color-scheme: dark)"
    srcset="https://history.starcat.ink/embed/v1/repos/starcat-app/Starcat/star-history.svg?theme=dark&amp;locale=zh">
  <img
    alt="starcat-app/Starcat 星标历史"
    src="https://history.starcat.ink/embed/v1/repos/starcat-app/Starcat/star-history.svg?theme=light&amp;locale=zh">
</picture>
```







![20260913173049_57z5NLDX](./docs/images/20260913173049_57z5NLDX.webp)





接口只接受 `theme=light|dark` 和 `locale=en|zh`，会验证仓库必须为公开仓库，并返回不依赖 JavaScript、远程样式或远程图片的可缓存 SVG。官方接口至少返回两个历史点后才会生成图片。

HTML 属性中的查询参数使用 `&amp;`，命令行 URL 使用普通 `&`。例如，直接获取 SVG 文件：

```bash
curl -fsS \
  'https://history.starcat.ink/embed/v1/repos/OWNER/REPO/star-history.svg?theme=light&locale=zh' \
  -o star-history.svg
```

本地启动服务后，将域名替换为 `http://127.0.0.1:5014` 即可测试；本地地址只能被当前电脑访问，不能直接用于 GitHub README。

## 安全边界

- 服务不保存 GitHub 身份、actor、事件 payload、Starcat 用户数据或私有/内部仓库数据。
- 服务不连接 BigQuery，也不读取任何本地 Raw 数据湖。
- GitHub Token 仅用于读取公开仓库当前 metadata 和官方公开 Star 历史；未配置时受 GitHub 匿名限额约束。
- 安全漏洞请通过 GitHub Security Advisories 私密报告，见 [SECURITY.md](./SECURITY.md)。

## 聚合部署

生产环境作为 `starcat-api` 的第七个模块运行，通过 `X-SC-Svc: history` 分流；无需新增独立 Fly App。独立二进制与 Dockerfile 仍保留，方便本地验证和第三方自托管。

## License

[MIT](./LICENSE)
