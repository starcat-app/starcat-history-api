# Starcat History Builder

本地数据构建器。它读取 GH Archive WatchEvent Parquet 或 Trainer Canonical Parquet，生成
`starcat-history-api` 可直接发布的 Snapshot / Delta ZIP；`daily` 子命令还会把单日
Raw → Silver → Delta → 幂等发布串成可重放任务。

Snapshot Builder 会在封包前执行完整 SQLite `PRAGMA quick_check`，并把校验结果与
数据库字节数写入 manifest v2。云端据此避免重复扫描 GiB 级 SQLite，同时继续独立
校验传输摘要、必要表和激活水位。

完整用法见仓库根目录 `README.md`。
