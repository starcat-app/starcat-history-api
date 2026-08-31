# 更新日志

## 0.1.1-dev

- 新增多日 History Delta 自动追赶，按生产水位连续构建、发布并支持失败续跑。
- 第三方曲线接口支持可选 `current_stars`：合法时跳过 GitHub metadata。
- 新增 Starcat 专用 `GET /api/v1/repos/{owner}/{repo}/star-history/events`，只返回原始日事件。
- 新增 `scripts/install-local-snapshot.sh` / `make install-local-snapshot`，支持本机只拷贝 `history.sqlite` 做联调。

## 0.1.0-dev

- 新增 GH Archive WatchEvent Silver、Snapshot 与 Delta Builder。
- 新增压缩 Star History Serving SQLite 与公开查询 API。
- 新增 Snapshot/Delta 校验、发布、幂等应用和版本回切。
- 支持作为 `starcat-api` 聚合模块或独立进程运行。
