// Package series 负责仓库日事件序列的压缩、校验与查询期转换。
package series

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"
)

const (
	// Encoding 是写入 Serving SQLite 的稳定编码名称。
	Encoding = "delta-uvarint-v1"
	// maxDecodedPoints 防止损坏 BLOB 诱导服务分配无限内存。
	maxDecodedPoints = 100_000
)

var (
	ErrCorruptSeries       = errors.New("corrupt history series")
	ErrUnsupportedEncoding = errors.New("unsupported history series encoding")
)

// DayCount 表示 UTC 日期上的 WatchEvent 数量。Day 是 Unix Epoch 起的天数。
type DayCount struct {
	Day   int
	Count uint64
}

// Merge 把按日增量合并进已有序列。相同日期累加，输出始终按日期严格递增。
func Merge(existing, additions []DayCount) ([]DayCount, error) {
	counts := make(map[int]uint64, len(existing)+len(additions))
	for _, values := range [][]DayCount{existing, additions} {
		for _, point := range values {
			if point.Day < 0 || point.Count == 0 {
				return nil, fmt.Errorf("%w: invalid merge point", ErrCorruptSeries)
			}
			current := counts[point.Day]
			if ^uint64(0)-current < point.Count {
				return nil, fmt.Errorf("%w: event count overflow", ErrCorruptSeries)
			}
			counts[point.Day] = current + point.Count
		}
	}
	days := make([]int, 0, len(counts))
	for day := range counts {
		days = append(days, day)
	}
	sort.Ints(days)
	result := make([]DayCount, 0, len(days))
	for _, day := range days {
		result = append(result, DayCount{Day: day, Count: counts[day]})
	}
	return result, nil
}

// DayFromTime 把时间归一为稳定的 UTC 日期编号。
func DayFromTime(value time.Time) int {
	utc := value.UTC()
	day := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	return int(day.Unix() / 86400)
}

// TimeFromDay 将日期编号恢复为 UTC 零点。
func TimeFromDay(day int) time.Time {
	return time.Unix(int64(day)*86400, 0).UTC()
}

// Checksum 返回序列原始字节的 SHA-256。
func Checksum(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// Encode 使用 unsigned varint 顺序写入「日期差、事件数」。
// 第一项日期差相对 Unix Epoch；后续项相对前一日期，因此十年日序列仍很紧凑。
func Encode(points []DayCount) ([]byte, string, error) {
	buffer := make([]byte, 0, len(points)*3)
	previousDay := 0
	for index, point := range points {
		if point.Day < 0 || point.Count == 0 {
			return nil, "", fmt.Errorf("%w: invalid point at index %d", ErrCorruptSeries, index)
		}
		if index > 0 && point.Day <= previousDay {
			return nil, "", fmt.Errorf("%w: dates must be strictly increasing", ErrCorruptSeries)
		}
		delta := point.Day
		if index > 0 {
			delta -= previousDay
		}
		var scratch [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(scratch[:], uint64(delta))
		buffer = append(buffer, scratch[:n]...)
		n = binary.PutUvarint(scratch[:], point.Count)
		buffer = append(buffer, scratch[:n]...)
		previousDay = point.Day
	}
	return buffer, Checksum(buffer), nil
}

// Decode 在读取时同时验证编码、checksum、点数与单调日期。
func Decode(encoding string, payload []byte, expectedChecksum string, expectedPoints int) ([]DayCount, error) {
	if encoding != Encoding {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedEncoding, encoding)
	}
	if expectedChecksum != "" && Checksum(payload) != expectedChecksum {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrCorruptSeries)
	}
	if expectedPoints < 0 || expectedPoints > maxDecodedPoints {
		return nil, fmt.Errorf("%w: invalid point count", ErrCorruptSeries)
	}
	points := make([]DayCount, 0, expectedPoints)
	previousDay := 0
	for offset := 0; offset < len(payload); {
		if len(points) >= maxDecodedPoints {
			return nil, fmt.Errorf("%w: too many points", ErrCorruptSeries)
		}
		delta, n := binary.Uvarint(payload[offset:])
		if n <= 0 {
			return nil, fmt.Errorf("%w: invalid day varint", ErrCorruptSeries)
		}
		offset += n
		count, n := binary.Uvarint(payload[offset:])
		if n <= 0 || count == 0 {
			return nil, fmt.Errorf("%w: invalid count varint", ErrCorruptSeries)
		}
		offset += n

		day64 := int64(delta)
		if len(points) > 0 {
			day64 += int64(previousDay)
		}
		if day64 < 0 || day64 > int64(^uint(0)>>1) {
			return nil, fmt.Errorf("%w: day overflow", ErrCorruptSeries)
		}
		day := int(day64)
		if len(points) > 0 && day <= previousDay {
			return nil, fmt.Errorf("%w: dates are not increasing", ErrCorruptSeries)
		}
		points = append(points, DayCount{Day: day, Count: count})
		previousDay = day
	}
	if expectedPoints != 0 && len(points) != expectedPoints {
		return nil, fmt.Errorf("%w: point count mismatch", ErrCorruptSeries)
	}
	return points, nil
}
