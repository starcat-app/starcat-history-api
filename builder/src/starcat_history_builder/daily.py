"""单日 WatchEvent 到 History Serving 的可重放增量编排。

BigQuery 下载属于本地数据平台职责；本模块从已落盘的单日 Raw 分区开始，生成可审计
Silver 与相邻水位 Delta，并在服务端确认当前水位后幂等发布。服务端水位始终是真实状态，
本地回执只用于运维审计，避免机器重启后误把本地状态当成生产事实。
"""

from __future__ import annotations

import http.client
import json
import os
import tempfile
from dataclasses import dataclass
from datetime import date, timedelta
from pathlib import Path
from typing import Any, Protocol
from urllib.parse import quote, urlsplit

from .build import DuckDBOptions, build_delta, build_silver, file_checksum, input_checksum


class HistoryPublisher(Protocol):
    """History Serving 发布端口，测试可替换为内存实现。"""

    def active(self) -> dict[str, Any]: ...

    def publish_delta(self, delta_id: str, archive: Path) -> dict[str, Any]: ...


class HTTPHistoryPublisher:
    """使用流式 HTTP 上传 Delta，避免把压缩包整体读入内存。"""

    def __init__(
        self,
        base_url: str,
        token: str,
        timeout_seconds: int = 600,
        gateway_service: str = "",
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.timeout_seconds = timeout_seconds
        self.gateway_service = gateway_service.strip()

    def active(self) -> dict[str, Any]:
        return self._request_json("GET", "/internal/v1/history-active")

    def publish_delta(self, delta_id: str, archive: Path) -> dict[str, Any]:
        return self._request_json(
            "POST",
            f"/internal/v1/history-deltas/{quote(delta_id, safe='')}",
            archive=archive,
        )

    def _request_json(self, method: str, suffix: str, archive: Path | None = None) -> dict[str, Any]:
        target = urlsplit(f"{self.base_url}{suffix}")
        if target.scheme not in {"http", "https"} or not target.hostname:
            raise ValueError("History base URL 必须是 http(s) URL")
        connection_type = http.client.HTTPSConnection if target.scheme == "https" else http.client.HTTPConnection
        connection = connection_type(target.hostname, target.port, timeout=self.timeout_seconds)
        path = target.path or "/"
        if target.query:
            path += f"?{target.query}"
        try:
            connection.putrequest(method, path)
            connection.putheader("Authorization", f"Bearer {self.token}")
            connection.putheader("Accept", "application/json")
            if self.gateway_service:
                # 聚合 starcat-api 的业务路径彼此冲突，必须用短请求头明确分流。
                connection.putheader("X-SC-Svc", self.gateway_service)
            if archive is not None:
                connection.putheader("Content-Type", "application/zip")
                connection.putheader("Content-Length", str(archive.stat().st_size))
            connection.endheaders()
            if archive is not None:
                with archive.open("rb") as stream:
                    for block in iter(lambda: stream.read(8 * 1024 * 1024), b""):
                        connection.send(block)
            response = connection.getresponse()
            payload = response.read()
            if response.status < 200 or response.status >= 300:
                detail = payload.decode("utf-8", errors="replace")[:2_000]
                raise RuntimeError(f"History 发布失败: HTTP {response.status}: {detail}")
            decoded = json.loads(payload or b"{}")
            if not isinstance(decoded, dict):
                raise RuntimeError("History 服务返回的 JSON 不是对象")
            return decoded
        finally:
            connection.close()


@dataclass(frozen=True)
class DailyOptions:
    """单日增量任务的本地目录与资源限制。"""

    inputs: list[str]
    silver_dir: Path
    delta_dir: Path
    target_watermark: str
    duckdb: DuckDBOptions


def run_daily(options: DailyOptions, publisher: HistoryPublisher) -> dict[str, Any]:
    """构建并发布一个与服务端当前水位相邻的单日增量。"""
    target = date.fromisoformat(options.target_watermark)
    active = publisher.active()
    active_watermark = str(active.get("active_watermark", ""))
    if not active_watermark:
        raise RuntimeError("History Serving 尚未激活 Snapshot，不能应用增量")
    current = date.fromisoformat(active_watermark)
    if current >= target:
        return {
            "status": "already_applied",
            "active_watermark": active_watermark,
            "target_watermark": options.target_watermark,
        }
    if target != current + timedelta(days=1):
        raise RuntimeError(
            f"History 增量水位不相邻: active={active_watermark} target={options.target_watermark}"
        )

    suffix = target.strftime("%Y%m%d")
    dataset_id = f"watch-silver-{suffix}-v1"
    delta_id = f"watch-delta-{suffix}-v1"
    silver = _ensure_silver(options, dataset_id, target)
    silver_inputs = [str(silver / "data" / "**" / "part_*.parquet")]
    archive = _ensure_delta(options, delta_id, active_watermark, silver_inputs)
    response = publisher.publish_delta(delta_id, archive)
    if str(response.get("active_watermark", "")) != options.target_watermark:
        raise RuntimeError("History 发布响应水位与目标水位不一致")
    receipt = {
        "schema_version": 1,
        "delta_id": delta_id,
        "target_watermark": options.target_watermark,
        "source_checksum": input_checksum(silver_inputs),
        "response": response,
    }
    _atomic_json(archive.parent / "publish-receipt.json", receipt)
    return {"status": "published", **receipt}


def _ensure_silver(options: DailyOptions, dataset_id: str, target: date) -> Path:
    destination = options.silver_dir.resolve() / dataset_id
    expected_checksum = input_checksum(options.inputs)
    if destination.exists():
        manifest = _load_manifest(destination / "manifest.json")
        _expect(manifest, "kind", "history_silver")
        _expect(manifest, "dataset_id", dataset_id)
        _expect(manifest, "source_watermark", target.isoformat())
        _expect(manifest, "input_checksum", expected_checksum)
    else:
        destination = build_silver(
            options.inputs,
            options.silver_dir,
            dataset_id,
            target.isoformat(),
            options.duckdb,
        )
        manifest = _load_manifest(destination / "manifest.json")
    target_day = (target - date(1970, 1, 1)).days
    if manifest.get("minimum_event_day") != target_day or manifest.get("maximum_event_day") != target_day:
        raise RuntimeError("单日 Raw 分区包含目标日期之外的数据，拒绝发布")
    for item in manifest.get("files", []):
        path = destination / str(item.get("path", ""))
        if not path.is_file() or file_checksum(path) != item.get("sha256"):
            raise RuntimeError(f"Silver 文件校验失败: {path}")
    return destination


def _ensure_delta(
    options: DailyOptions,
    delta_id: str,
    from_watermark: str,
    silver_inputs: list[str],
) -> Path:
    destination = options.delta_dir.resolve() / delta_id
    expected_checksum = input_checksum(silver_inputs)
    if destination.exists():
        manifest = _load_manifest(destination / "manifest.json")
        _expect(manifest, "kind", "history_delta")
        _expect(manifest, "delta_id", delta_id)
        _expect(manifest, "from_watermark", from_watermark)
        _expect(manifest, "to_watermark", options.target_watermark)
        _expect(manifest, "source_checksum", expected_checksum)
        archive = destination / f"{delta_id}.zip"
        if not archive.is_file():
            raise RuntimeError(f"Delta ZIP 不存在: {archive}")
        return archive
    return build_delta(
        silver_inputs,
        options.delta_dir,
        delta_id,
        from_watermark,
        options.target_watermark,
        options.duckdb,
    )


def _load_manifest(path: Path) -> dict[str, Any]:
    payload = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(payload, dict):
        raise RuntimeError(f"manifest 不是 JSON 对象: {path}")
    return payload


def _expect(manifest: dict[str, Any], field: str, expected: Any) -> None:
    if manifest.get(field) != expected:
        raise RuntimeError(f"已有产物 {field} 不匹配，拒绝覆盖或重用")


def _atomic_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            json.dump(payload, stream, ensure_ascii=False, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
