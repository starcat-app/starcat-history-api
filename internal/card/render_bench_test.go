package card

import (
	"fmt"
	"testing"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/model"
)

// 渲染成本必须随历史长度近似线性：README 卡片是公开图片入口，长历史仓库
// （golang/go 这类 600+ 周）曾经在热缓存下也要 1 秒以上 CPU，而热路径本该在毫秒级。
// 这个基准就是那条回归线：一旦有人写出 O(n²) 的点级循环，这里的数字会立刻跳。
func BenchmarkRender(b *testing.B) {
	for _, days := range []int{120, 1000, 4410} {
		input := syntheticRenderInput(days)
		b.Run(fmt.Sprintf("days=%d", days), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := Render(input); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func syntheticRenderInput(days int) RenderInput {
	start := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
	points := make([]model.HistoryPoint, 0, days)
	count := 0
	for index := 0; index < days; index++ {
		date := start.AddDate(0, 0, index)
		// 递增的星标数：让里程碑、增长事件这些依赖数值的路径真的被走到。
		if index%7 == 0 {
			count += 3 + index/30
		}
		points = append(points, model.HistoryPoint{
			Date:      date.Format("2006-01-02"),
			Count:     count,
			Source:    "github_history",
			Precision: "reconstructed",
		})
	}
	return RenderInput{
		FullName:      "owner/repo",
		Description:   "a synthetic repository used by the render benchmark",
		Language:      "Go",
		Topics:        []string{"history", "metrics", "svg"},
		CurrentStars:  count,
		CreatedAt:     start,
		CoverageStart: start,
		GeneratedAt:   start.AddDate(0, 0, days),
		AvatarDataURI: "",
		Points:        points,
		Theme:         ThemeLight,
		Locale:        LocaleEnglish,
	}
}
