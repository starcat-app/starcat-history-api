// Package version 保存服务对外暴露的稳定版本信息。
package version

const (
	// Service 是聚合网关和运维指标使用的服务标识。
	Service = "history"
	// Version 是当前开发版本，正式发布时由发布流程统一更新。
	Version = "0.1.0-dev"
)
