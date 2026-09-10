package series

import (
	"fmt"
	"sort"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/model"
)

const DefaultMaximumPoints = 400

// Normalize 把 WatchEvent 累计比例校准到 GitHub 当前 stars。
// GH Archive 不含可靠的 unstar 反向事件，因此这里保持与旧 Discovery 实现一致：
// 只提供单调、估算曲线，并强制最后一个点等于 GitHub 当前值。
func Normalize(events []DayCount, currentStars int) ([]model.HistoryPoint, error) {
	return normalize(events, currentStars, "gh_archive", "estimated")
}

// NormalizeOfficial 把 GitHub 官方每日新增 Star 重建为累计曲线。
// 官方接口提供真实的新增事件，但不提供 unstar 反向历史，因此仍需用当前 stars
// 作为末端锚点；source/precision 明确告诉客户端这是官方历史的重建曲线。
func NormalizeOfficial(events []DayCount, currentStars int) ([]model.HistoryPoint, error) {
	return normalize(events, currentStars, "github_history", "reconstructed")
}

func normalize(events []DayCount, currentStars int, source, precision string) ([]model.HistoryPoint, error) {
	if currentStars < 0 {
		return nil, fmt.Errorf("current_stars must not be negative")
	}
	if len(events) == 0 {
		return []model.HistoryPoint{}, nil
	}
	var total uint64
	for _, event := range events {
		if event.Count == 0 {
			return nil, fmt.Errorf("event count must be positive")
		}
		total += event.Count
	}
	if total == 0 {
		return []model.HistoryPoint{}, nil
	}
	points := make([]model.HistoryPoint, 0, len(events))
	var cumulative uint64
	previous := 0
	for _, event := range events {
		cumulative += event.Count
		numerator := uint64(currentStars) * cumulative
		estimated := int((2*numerator + total) / (2 * total))
		if estimated < previous {
			estimated = previous
		}
		if estimated > currentStars {
			estimated = currentStars
		}
		points = append(points, model.HistoryPoint{
			Date:      TimeFromDay(event.Day).Format("2006-01-02"),
			Count:     estimated,
			Source:    source,
			Precision: precision,
		})
		previous = estimated
	}
	points[len(points)-1].Count = currentStars
	return points, nil
}

// SelectRange 保持旧接口的降采样语义：3m 按日、1y 按周、all 按月，
// 并在超出上限时等距保留首尾点，避免前端一次渲染数千个节点。
func SelectRange(points []model.HistoryPoint, historyRange model.HistoryRange, now time.Time, maximum int) ([]model.HistoryPoint, error) {
	if !historyRange.Valid() {
		return nil, fmt.Errorf("unsupported history range %q", historyRange)
	}
	if maximum <= 0 {
		maximum = DefaultMaximumPoints
	}
	type dated struct {
		point model.HistoryPoint
		date  time.Time
	}
	values := make([]dated, 0, len(points))
	for _, point := range points {
		date, err := time.Parse("2006-01-02", point.Date)
		if err != nil {
			return nil, fmt.Errorf("invalid point date %q: %w", point.Date, err)
		}
		values = append(values, dated{point: point, date: date})
	}
	sort.Slice(values, func(i, j int) bool { return values[i].date.Before(values[j].date) })

	var cutoff time.Time
	bucket := func(date time.Time) string { return date.Format("2006-01-02") }
	switch historyRange {
	case model.HistoryRangeThreeMonths:
		cutoff = now.UTC().AddDate(0, -3, 0)
	case model.HistoryRangeOneYear:
		cutoff = now.UTC().AddDate(-1, 0, 0)
		bucket = func(date time.Time) string {
			year, week := date.ISOWeek()
			return fmt.Sprintf("%04d-W%02d", year, week)
		}
	case model.HistoryRangeAll:
		bucket = func(date time.Time) string { return date.Format("2006-01") }
	}
	if !cutoff.IsZero() {
		cutoff = time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC)
	}

	grouped := make([]model.HistoryPoint, 0, len(values))
	lastBucket := ""
	for _, value := range values {
		if !cutoff.IsZero() && value.date.Before(cutoff) {
			continue
		}
		key := bucket(value.date)
		if key == lastBucket {
			grouped[len(grouped)-1] = value.point
		} else {
			grouped = append(grouped, value.point)
			lastBucket = key
		}
	}
	if len(grouped) <= maximum {
		return grouped, nil
	}
	result := make([]model.HistoryPoint, 0, maximum)
	for index := 0; index < maximum; index++ {
		sourceIndex := index * (len(grouped) - 1) / (maximum - 1)
		result = append(result, grouped[sourceIndex])
	}
	return result, nil
}
