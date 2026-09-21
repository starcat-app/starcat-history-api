package card

import (
	"testing"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/model"
)

// parseDate 是渲染热路径上调用最密集的函数（一次渲染可达几十万次），因此它被换成
// 了手写解析。这里把"与原实现语义完全一致"固定下来：合法日期、越界日期、非日期
// 字符串三种情况的行为都不能变 —— 上层靠"零值 = 非法日期"拒绝脏数据。
func TestParseDateMatchesTimeParseSemantics(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Time // 零值表示必须解析失败
	}{
		{"普通日期", "2026-09-15", time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)},
		{"年初", "2014-01-01", time.Date(2014, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"闰年 2 月 29 日", "2024-02-29", time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)},
		{"平年 2 月 29 日必须非法", "2023-02-29", time.Time{}},
		{"2 月 30 日必须非法", "2026-02-30", time.Time{}},
		{"月份越界必须非法", "2026-13-01", time.Time{}},
		{"日为 0 必须非法", "2026-01-00", time.Time{}},
		{"日越界必须非法", "2026-01-32", time.Time{}},
		{"非数字必须非法", "20xx-01-01", time.Time{}},
		{"长度不符必须非法", "2026-1-1", time.Time{}},
		{"空串必须非法", "", time.Time{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := parseDate(testCase.raw)
			if testCase.want.IsZero() {
				if !got.IsZero() {
					t.Fatalf("parseDate(%q) = %s, want zero time", testCase.raw, got)
				}
				return
			}
			if !got.Equal(testCase.want) {
				t.Fatalf("parseDate(%q) = %s, want %s", testCase.raw, got, testCase.want)
			}
		})
	}
}

// dayNumber / dateFromDay 必须互为逆运算：二分查找与 x 坐标都依赖这个一致性。
func TestDayNumberRoundTripsThroughDateFromDay(t *testing.T) {
	for _, raw := range []string{"1970-01-01", "2014-01-01", "2026-09-15", "2026-12-31"} {
		day := dayNumber(parseDate(raw))
		if got := dateFromDay(day).Format("2006-01-02"); got != raw {
			t.Fatalf("round trip %q → day %d → %q", raw, day, got)
		}
	}
}

// valueAtOrBefore 从线性扫描换成了二分查找，语义必须逐点一致：
// 返回日序号 <= target 的最后一个点；target 早于所有点时不返回点。
func TestValueAtOrBeforeMatchesLinearScan(t *testing.T) {
	points := []model.HistoryPoint{
		{Date: "2026-01-01", Count: 1},
		{Date: "2026-01-05", Count: 2},
		{Date: "2026-01-06", Count: 3},
		{Date: "2026-02-01", Count: 4},
	}
	days := pointDays(points)

	linearScan := func(day int) (int, bool) {
		for index := len(points) - 1; index >= 0; index-- {
			if dayNumber(parseDate(points[index].Date)) <= day {
				return points[index].Count, true
			}
		}
		return 0, false
	}

	// 覆盖：早于全部、恰好命中、落在两点之间、晚于全部。
	for _, day := range []int{
		dayNumber(parseDate("2025-12-31")),
		dayNumber(parseDate("2026-01-01")),
		dayNumber(parseDate("2026-01-05")),
		dayNumber(parseDate("2026-01-07")),
		dayNumber(parseDate("2026-03-01")),
	} {
		wantCount, wantOK := linearScan(day)
		point, ok := valueAtOrBefore(points, days, day)
		if ok != wantOK {
			t.Fatalf("day %d: ok=%v, want %v", day, ok, wantOK)
		}
		if ok && point.Count != wantCount {
			t.Fatalf("day %d: count=%d, want %d", day, point.Count, wantCount)
		}
	}
}

// lastPointAtOrBefore 接收的是时间而不是日序号，边界同样要一致。
func TestLastPointAtOrBeforeBounds(t *testing.T) {
	points := []model.HistoryPoint{
		{Date: "2026-01-01", Count: 1},
		{Date: "2026-01-10", Count: 2},
	}
	days := pointDays(points)
	if point := lastPointAtOrBefore(points, days, time.Date(2026, 1, 9, 12, 0, 0, 0, time.UTC)); point == nil || point.Count != 1 {
		t.Fatalf("expected the first point, got %+v", point)
	}
	if point := lastPointAtOrBefore(points, days, time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)); point != nil {
		t.Fatalf("expected nil before the first point, got %+v", point)
	}
}
