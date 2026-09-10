// Package series 中的官方历史转换逻辑把 GitHub 周数据还原成 Starcat 使用的日序列。
package series

import (
	"fmt"
	"sort"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/model"
)

// OfficialEvents 将 GitHub 每周 days 展开为按 UTC 日期聚合的新增事件。
// 官方接口按周返回，且同一周可能因分页合并重复出现；先按 week 规范化，再展开，
// 保证 DB 合并和 ETag 刷新不会重复计算某一天的 Star。
func OfficialEvents(weeks []model.GitHubStarHistoryWeek) ([]DayCount, error) {
	canonical := make(map[int64]model.GitHubStarHistoryWeek, len(weeks))
	for _, week := range weeks {
		if week.Week <= 0 || len(week.Days) == 0 || len(week.Days) > 7 {
			return nil, fmt.Errorf("invalid github star history week %d", week.Week)
		}
		if week.Total < 0 {
			return nil, fmt.Errorf("negative github star history total for week %d", week.Week)
		}
		for _, count := range week.Days {
			if count < 0 {
				return nil, fmt.Errorf("negative github star history day count for week %d", week.Week)
			}
		}
		canonical[week.Week] = week
	}
	keys := make([]int64, 0, len(canonical))
	for week := range canonical {
		keys = append(keys, week)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	result := make([]DayCount, 0)
	for _, key := range keys {
		week := canonical[key]
		start := time.Unix(week.Week, 0).UTC()
		for offset, count := range week.Days {
			if count == 0 {
				continue
			}
			result = append(result, DayCount{
				Day:   DayFromTime(start.AddDate(0, 0, offset)),
				Count: uint64(count),
			})
		}
	}
	return Merge(nil, result)
}
