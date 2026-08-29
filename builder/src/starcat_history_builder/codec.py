"""与 Go Serving 完全一致的 delta-uvarint-v1 编码。"""

from __future__ import annotations

import hashlib
from collections.abc import Iterable

ENCODING = "delta-uvarint-v1"


def _uvarint(value: int) -> bytes:
    """编码非负整数；拒绝负值，避免 Python 的无限精度掩盖数据错误。"""
    if value < 0:
        raise ValueError("uvarint value must not be negative")
    output = bytearray()
    while value >= 0x80:
        output.append((value & 0x7F) | 0x80)
        value >>= 7
    output.append(value)
    return bytes(output)


def encode(points: Iterable[tuple[int, int]]) -> tuple[bytes, str, int, int]:
    """编码严格递增的 (event_day, event_count)，并返回 BLOB/checksum/点数/总事件数。"""
    payload = bytearray()
    previous_day = 0
    point_count = 0
    event_total = 0
    for day, count in points:
        if day < 0 or count <= 0:
            raise ValueError("event day and count must be positive")
        if point_count and day <= previous_day:
            raise ValueError("event days must be strictly increasing")
        delta = day if point_count == 0 else day - previous_day
        payload.extend(_uvarint(delta))
        payload.extend(_uvarint(count))
        point_count += 1
        event_total += count
        previous_day = day
    raw = bytes(payload)
    return raw, hashlib.sha256(raw).hexdigest(), point_count, event_total

