// Package card 将已校准的 Star History 数据渲染为自包含 SVG。
//
// 这里复刻 Starcat README 星标历史卡片的计算口径，而不是只复刻一张静态截图：
// 曲线抽稀、90 天指标、自然坐标轴和 Star Journey 必须随仓库数据一起变化。
package card

import (
	"encoding/base64"
	"fmt"
	"html"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/model"
)

const (
	// RendererVersion 参与公开 SVG 的 ETag；改变布局或计算口径时必须递增。
	RendererVersion    = "star-history-svg-v5"
	maximumDrawnPoints = 90

	canvasWidth   = 1200.0
	canvasHeight  = 1060.0
	cardX         = 20.0
	cardY         = 20.0
	cardWidth     = 1160.0
	cardHeight    = 1020.0
	chartLeft     = 100.0
	chartRight    = 1120.0
	chartTop      = 276.0
	chartBottom   = 600.0
	calloutWidth  = 144.0
	calloutHeight = 60.0
	calloutGap    = 14.0
)

// Theme 是公开 SVG 允许的视觉主题。
type Theme string

const (
	ThemeLight Theme = "light"
	ThemeDark  Theme = "dark"
)

// Locale 是公开 SVG 允许的文案语言。
type Locale string

const (
	LocaleEnglish Locale = "en"
	LocaleChinese Locale = "zh"
)

// RenderInput 是 Renderer 的完整输入；调用方必须先完成 Public 门禁和 Star 校准。
// CreatedAt 只参与创建时长、创建日基线和 Journey，CoverageStart 只用于指标覆盖门禁。
type RenderInput struct {
	FullName      string
	Description   string
	Language      string
	Topics        []string
	CurrentStars  int
	CreatedAt     time.Time
	CoverageStart time.Time
	GeneratedAt   time.Time
	AvatarDataURI string
	Points        []model.HistoryPoint
	Theme         Theme
	Locale        Locale
}

// Render 生成与 Starcat README 卡片同结构的固定 viewBox SVG。
//
// SVG 不携带 JavaScript 或远程资源，因此 README 图片代理可以直接缓存。交互式
// hover 无法在静态图片中保留，Renderer 用 Starcat 同口径的最多 4 个事件 callout
// 表达最重要的节点，并将完整日期、指标和 Journey 文案放入可访问文本中。
func Render(input RenderInput) ([]byte, error) {
	if strings.TrimSpace(input.FullName) == "" {
		return nil, fmt.Errorf("full name is required")
	}
	if input.CurrentStars < 0 {
		return nil, fmt.Errorf("current stars must not be negative")
	}
	if input.Theme != ThemeLight && input.Theme != ThemeDark {
		return nil, fmt.Errorf("unsupported theme %q", input.Theme)
	}
	if input.Locale != LocaleEnglish && input.Locale != LocaleChinese {
		return nil, fmt.Errorf("unsupported locale %q", input.Locale)
	}

	points, err := preparePoints(input.Points)
	if err != nil {
		return nil, err
	}
	if len(points) < 2 {
		return nil, fmt.Errorf("at least two history points are required")
	}
	if points[len(points)-1].Count != input.CurrentStars {
		return nil, fmt.Errorf("last history point must equal current stars")
	}

	labels := labelsFor(input.Locale)
	palette := paletteFor(input.Theme)
	// 日序号只算一次：它是"按日查询历史点"和绘制 x 坐标的共同输入，
	// 逐点重算是渲染长历史时的主要成本。
	days := pointDays(points)
	plottingPoints := addingCreationBaseline(points, input.CreatedAt)
	journey := buildJourney(points, days, input.CreatedAt, input.CurrentStars, input.CoverageStart)
	rendered := renderedPointsWithAnchors(plottingPoints, journey.chartEvents)
	axis := newAxis(maximumCount(points))
	metrics := buildMetrics(points, days, input.CreatedAt, input.CoverageStart, input.GeneratedAt)
	fullName := strings.TrimSpace(input.FullName)
	currentStars := compactNumber(input.CurrentStars)
	updated := "—"
	if !input.GeneratedAt.IsZero() {
		updated = formatDate(input.GeneratedAt, input.Locale, true)
	}

	var svg strings.Builder
	svg.Grow(32_000)
	fmt.Fprintf(&svg, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" role="img" aria-labelledby="starcat-history-title starcat-history-description">`, canvasWidth, canvasHeight)
	fmt.Fprintf(&svg, `<title id="starcat-history-title">%s</title>`, escapeXML(fullName+" "+labels.kicker))
	fmt.Fprintf(&svg, `<desc id="starcat-history-description">%s</desc>`, escapeXML(accessibilityDescription(fullName, input.CurrentStars, input.Locale)))
	fmt.Fprintf(&svg, `<rect width="%.0f" height="%.0f" rx="32" fill="%s"/>`, canvasWidth, canvasHeight, palette.background)
	fmt.Fprintf(&svg, `<rect x="%.0f" y="%.0f" width="%.0f" height="%.0f" rx="28" fill="%s" stroke="%s" stroke-width="1.5"/>`, cardX, cardY, cardWidth, cardHeight, palette.panel, palette.border)
	svg.WriteString(`<defs><clipPath id="starcat-history-avatar-clip"><rect x="54" y="66" width="82" height="82" rx="22"/></clipPath></defs>`)

	renderHeader(&svg, input, labels, palette, fullName, currentStars)
	renderChart(&svg, plottingPoints, rendered, journey.chartEvents, axesFor(plottingPoints, axis, input.Locale), axis, palette, input.Locale)
	renderMetrics(&svg, metrics, input.Locale, palette)
	renderJourney(&svg, journey, input.Locale, palette)
	renderFooter(&svg, updated, labels, palette)

	svg.WriteString(`</svg>`)
	return []byte(svg.String()), nil
}

type cardLabels struct {
	kicker, totalStars, dailyAverage, age, newStars, growth, journey, updated, poweredBy string
}

func labelsFor(locale Locale) cardLabels {
	if locale == LocaleChinese {
		return cardLabels{
			kicker: "GitHub Star History", totalStars: "总 Stars",
			dailyAverage: "日均新增", age: "创建时长", newStars: "期间新增", growth: "期间增幅",
			journey: "Star Journey", updated: "更新于", poweredBy: "Powered by",
		}
	}
	return cardLabels{
		kicker: "GitHub Star History", totalStars: "Total Stars",
		dailyAverage: "Daily Average", age: "Repository Age", newStars: "New Stars", growth: "Growth",
		journey: "Star Journey", updated: "Updated", poweredBy: "Powered by",
	}
}

type palette struct {
	background, panel, inner, border, grid, text, secondary, line, fill, chip string
	green, purple, pink, gold, accent, growth, brand                          string
}

func paletteFor(theme Theme) palette {
	if theme == ThemeDark {
		return palette{
			background: "#0e131b", panel: "#171c23", inner: "#1d232c", border: "#303945", grid: "#39434f",
			text: "#f2f4f8", secondary: "#adb8ca", line: "#30d875", fill: "#30d87520", chip: "#252d38",
			green: "#35d990", purple: "#b293ff", pink: "#ff77b6", gold: "#ffd34d", accent: "#6ba7ff", growth: "#ff927a", brand: "#ffd34d",
		}
	}
	return palette{
		background: "#f4f7fb", panel: "#ffffff", inner: "#ffffff", border: "#e5eaf2", grid: "#dde5ef",
		text: "#101725", secondary: "#677791", line: "#08bd59", fill: "#08bd5926", chip: "#eef1f6",
		green: "#12bc75", purple: "#8453ff", pink: "#f44895", gold: "#f4bb21", accent: "#2580ff", growth: "#e96b4d", brand: "#9a6b00",
	}
}

func renderHeader(svg *strings.Builder, input RenderInput, labels cardLabels, palette palette, fullName, currentStars string) {
	// 当前 Serving 库只保证公开元数据，不强制下载远端头像；用 owner 首字母保留与 Starcat 相同的头像占位尺寸。
	avatarDataURI := safeAvatarDataURI(input.AvatarDataURI)
	fmt.Fprintf(svg, `<rect x="54" y="66" width="82" height="82" rx="22" fill="%s"/>`, palette.chip)
	if avatarDataURI != "" {
		fmt.Fprintf(svg, `<image x="54" y="66" width="82" height="82" preserveAspectRatio="xMidYMid slice" clip-path="url(#starcat-history-avatar-clip)" href="%s"/>`, escapeXML(avatarDataURI))
	} else {
		fmt.Fprintf(svg, `<text x="95" y="122" text-anchor="middle" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="30" font-weight="700" fill="%s">%s</text>`, palette.secondary, escapeXML(ownerInitial(fullName)))
	}
	fmt.Fprintf(svg, `<text x="160" y="89" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="21" font-weight="500" fill="%s">%s</text>`, palette.secondary, escapeXML(labels.kicker))
	fmt.Fprintf(svg, `<text x="160" y="130" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="31" font-weight="700" fill="%s">%s</text>`, palette.text, escapeXML(fullName))
	if description := truncateText(input.Description, 82); description != "" {
		fmt.Fprintf(svg, `<text x="160" y="166" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="18" fill="%s">%s</text>`, palette.secondary, escapeXML(description))
	}
	renderTags(svg, input.Language, input.Topics, palette)
	// 星形图标必须根据右侧数字宽度左移，否则 100K 以上的 compact 文案会与图标重叠。
	starX := 1120 - float64(len([]rune(currentStars))*25) - 42
	renderStar(svg, math.Max(930, starX), 128, 19, palette.gold)
	fmt.Fprintf(svg, `<text x="1120" y="139" text-anchor="end" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="42" font-weight="700" fill="%s">%s</text>`, palette.text, escapeXML(currentStars))
	fmt.Fprintf(svg, `<text x="1120" y="177" text-anchor="end" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="20" fill="%s">%s</text>`, palette.secondary, escapeXML(labels.totalStars))
}

func renderTags(svg *strings.Builder, language string, topics []string, palette palette) {
	items := make([]struct{ text, color string }, 0, 4)
	if strings.TrimSpace(language) != "" {
		items = append(items, struct{ text, color string }{strings.TrimSpace(language), "#f4513a"})
	}
	for index, topic := range topics {
		if index >= 3 || strings.TrimSpace(topic) == "" {
			break
		}
		items = append(items, struct{ text, color string }{strings.TrimSpace(topic), []string{palette.purple, palette.pink, palette.green}[index]})
	}
	x := 160.0
	for _, item := range items {
		width := math.Min(190, math.Max(76, float64(len([]rune(item.text))*10+42)))
		fmt.Fprintf(svg, `<rect x="%.1f" y="185" width="%.1f" height="30" rx="15" fill="%s"/><circle cx="%.1f" cy="200" r="5" fill="%s"/><text x="%.1f" y="206" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="16" fill="%s">%s</text>`, x, width, palette.chip, x+18, item.color, x+31, palette.secondary, escapeXML(truncateText(item.text, 22)))
		x += width + 8
	}
}

type axis struct {
	step, maximum float64
}

func newAxis(peak int) axis {
	target := math.Max(1, float64(peak)/4)
	magnitude := math.Pow(10, math.Floor(math.Log10(target)))
	for _, candidate := range []float64{1, 1.5, 2, 2.5, 3, 4, 5, 7.5, 10} {
		if candidate*magnitude >= target {
			step := math.Max(1, math.Ceil(candidate*magnitude))
			return axis{step: step, maximum: step * 4}
		}
	}
	step := math.Max(1, math.Ceil(10*magnitude))
	return axis{step: step, maximum: step * 4}
}

func axesFor(points []model.HistoryPoint, axis axis, locale Locale) []axisLabel {
	start := parseDate(points[0].Date)
	end := parseDate(points[len(points)-1].Date)
	duration := end.Sub(start)
	result := make([]axisLabel, 0, 6)
	for index := 0; index <= 5; index++ {
		date := start.Add(duration * time.Duration(index) / 5)
		result = append(result, axisLabel{date: date, text: formatAxisDate(date, duration, locale)})
	}
	return result
}

type axisLabel struct {
	date time.Time
	text string
}

func renderChart(svg *strings.Builder, points, rendered []model.HistoryPoint, events []journeyEvent, labels []axisLabel, axis axis, palette palette, locale Locale) {
	start := parseDate(points[0].Date)
	end := parseDate(points[len(points)-1].Date)
	duration := math.Max(1, end.Sub(start).Seconds())
	x := func(date time.Time) float64 {
		return chartLeft + date.Sub(start).Seconds()/duration*(chartRight-chartLeft)
	}
	y := func(count int) float64 { return chartBottom - float64(count)/axis.maximum*(chartBottom-chartTop) }
	coordinates := make([]coordinate, 0, len(rendered))
	for _, point := range rendered {
		coordinates = append(coordinates, coordinate{x: x(parseDate(point.Date)), y: y(point.Count)})
	}
	if len(coordinates) < 2 {
		return
	}
	gradientID := "starcat-history-fill"
	fmt.Fprintf(svg, `<defs><linearGradient id="%s" x1="0" y1="0" x2="0" y2="1"><stop offset="0%%" stop-color="%s" stop-opacity=".27"/><stop offset="100%%" stop-color="%s" stop-opacity=".035"/></linearGradient></defs>`, gradientID, palette.line, palette.line)
	for tick := 0; tick <= 4; tick++ {
		value := axis.step * float64(tick)
		tickY := y(int(value))
		fmt.Fprintf(svg, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1" stroke-dasharray="4 6"/>`, chartLeft, tickY, chartRight, tickY, palette.grid)
		fmt.Fprintf(svg, `<text x="%.1f" y="%.1f" text-anchor="end" dominant-baseline="middle" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="18" fill="%s">%s</text>`, chartLeft-14, tickY, palette.secondary, escapeXML(compactNumber(int(value))))
	}
	pointsText := coordinateText(coordinates)
	base := chartBottom
	fmt.Fprintf(svg, `<polygon points="%.1f,%.1f %s %.1f,%.1f" fill="url(#%s)"/>`, coordinates[0].x, base, pointsText, coordinates[len(coordinates)-1].x, base, gradientID)
	fmt.Fprintf(svg, `<polyline points="%s" fill="none" stroke="%s" stroke-width="4" stroke-linecap="round" stroke-linejoin="round"/>`, pointsText, palette.line)

	// Starcat 的 HTML 版本会以真实卡片尺寸计算位置，并在发生碰撞时放弃冲突标注。
	// SVG 没有 DOM 的 offsetWidth/offsetHeight，因此这里使用固定字体和尺寸对应的
	// 卡片盒子，在多个候选方位中选择第一个不重叠的位置，保证静态图片每次一致。
	placements := make([]calloutPlacement, len(events))
	visibleCallouts := make([]bool, len(events))
	for index, event := range events {
		// 最后一个点只承担“当前状态”标记；曲线卡片只展示中间历史事件，
		// 避免重复显示右上角已经展示过的当前 Stars。
		if event.point == nil || event.kind == journeyCurrent {
			continue
		}
		point := coordinate{x: x(event.pointDate), y: y(event.point.Count)}
		occupied := make([]calloutPlacement, 0, len(placements))
		for previousIndex, visible := range visibleCallouts {
			if visible {
				occupied = append(occupied, placements[previousIndex])
			}
		}
		placement, ok := placeCallout(point, occupied)
		if ok {
			placements[index] = placement
			visibleCallouts[index] = true
		}
	}
	for index, event := range events {
		if event.point == nil {
			continue
		}
		pointX, pointY := x(event.pointDate), y(event.point.Count)
		color := palette.accent
		if event.kind == journeySpike || event.kind == journeyBestDay || event.kind == journeyBestWeek {
			color = palette.growth
		}
		if event.kind == journeyFirstRecorded {
			color = palette.green
		}
		fmt.Fprintf(svg, `<circle cx="%.1f" cy="%.1f" r="6" fill="%s" stroke="%s" stroke-width="3"/>`, pointX, pointY, color, palette.panel)
		if event.kind == journeyCurrent {
			fmt.Fprintf(svg, `<circle cx="%.1f" cy="%.1f" r="10" fill="none" stroke="%s" stroke-width="2"/>`, pointX, pointY, palette.accent)
		}
		if event.kind != journeyCurrent && visibleCallouts[index] {
			placement := placements[index]
			fmt.Fprintf(svg, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1.5" stroke-dasharray="3 3"/>`, pointX, pointY, placement.anchorX(pointX), placement.anchorY(pointY), palette.border)
			fmt.Fprintf(svg, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="12" fill="%s" stroke="%s" stroke-width="1.5"/>`, placement.x, placement.y, calloutWidth, calloutHeight, palette.panel, palette.border)
			fmt.Fprintf(svg, `<text x="%.1f" y="%.1f" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="20" font-weight="700" fill="%s">%s</text>`, placement.x+14, placement.y+25, palette.text, escapeXML(compactNumber(event.point.Count)))
			fmt.Fprintf(svg, `<text x="%.1f" y="%.1f" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="16" fill="%s">%s</text>`, placement.x+14, placement.y+47, palette.secondary, escapeXML(formatDate(event.pointDate, locale, false)))
		}
	}
	last := coordinates[len(coordinates)-1]
	fmt.Fprintf(svg, `<circle cx="%.1f" cy="%.1f" r="8" fill="%s" stroke="%s" stroke-width="3"/>`, last.x, last.y, palette.line, palette.panel)
	for index, label := range labels {
		anchor := "middle"
		if index == 0 {
			anchor = "start"
		} else if index == len(labels)-1 {
			anchor = "end"
		}
		fmt.Fprintf(svg, `<text x="%.1f" y="637" text-anchor="%s" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="18" fill="%s">%s</text>`, x(label.date), anchor, palette.secondary, escapeXML(label.text))
	}
}

type calloutPlacement struct {
	x, y float64
}

// placeCallout 复刻 Starcat 的“点上方 + 边界夹紧 + 碰撞淘汰”规则，并为静态 SVG
// 增加左右和下方候选位置。候选顺序固定，避免同一数据在不同请求中产生跳动布局。
func placeCallout(point coordinate, occupied []calloutPlacement) (calloutPlacement, bool) {
	const (
		leftInset   = chartLeft + 4
		rightInset  = chartRight - calloutWidth - 4
		topInset    = chartTop - calloutHeight - 2
		bottomInset = chartBottom - calloutHeight - 2
	)
	candidates := []calloutPlacement{
		{x: point.x - calloutWidth/2, y: point.y - calloutHeight - calloutGap},
		{x: point.x - calloutWidth - calloutGap, y: point.y - calloutHeight - calloutGap},
		{x: point.x + calloutGap, y: point.y - calloutHeight - calloutGap},
		{x: point.x - calloutWidth/2, y: point.y - 2*(calloutHeight+calloutGap)},
		{x: point.x - calloutWidth/2, y: point.y - 3*(calloutHeight+calloutGap)},
		{x: point.x - calloutWidth - calloutGap, y: point.y - calloutHeight/2},
		{x: point.x + calloutGap, y: point.y - calloutHeight/2},
		{x: point.x - calloutWidth/2, y: point.y + calloutGap},
		{x: point.x - calloutWidth - calloutGap, y: point.y + calloutGap},
		{x: point.x + calloutGap, y: point.y + calloutGap},
		{x: point.x - calloutWidth/2, y: point.y + 2*(calloutHeight+calloutGap)},
	}
	for _, candidate := range candidates {
		candidate.x = math.Max(leftInset, math.Min(rightInset, candidate.x))
		candidate.y = math.Max(topInset, math.Min(bottomInset, candidate.y))
		if !calloutOverlaps(candidate, occupied) {
			return candidate, true
		}
	}
	return calloutPlacement{}, false
}

func calloutOverlaps(candidate calloutPlacement, occupied []calloutPlacement) bool {
	for _, other := range occupied {
		if candidate.x < other.x+calloutWidth+12 && candidate.x+calloutWidth+12 > other.x &&
			candidate.y < other.y+calloutHeight+8 && candidate.y+calloutHeight+8 > other.y {
			return true
		}
	}
	return false
}

func (placement calloutPlacement) anchorX(pointX float64) float64 {
	return math.Max(placement.x+12, math.Min(placement.x+calloutWidth-12, pointX))
}

func (placement calloutPlacement) anchorY(pointY float64) float64 {
	if pointY >= placement.y+calloutHeight {
		return placement.y + calloutHeight
	}
	return placement.y
}

func renderMetrics(svg *strings.Builder, metrics historyMetrics, locale Locale, palette palette) {
	labels := labelsFor(locale)
	values := []struct {
		value, label, color string
		icon                metricIcon
	}{
		{formatDailyAverage(metrics.dailyAverage), labels.dailyAverage, palette.green, metricBars},
		{formatAge(metrics.ageDays, locale), labels.age, palette.purple, metricCalendar},
		{formatSigned(metrics.growth), labels.newStars, palette.gold, metricStar},
		{formatRate(metrics.growthRate), labels.growth, palette.pink, metricGrowth},
	}
	width := 266.0
	for index, item := range values {
		x := 54 + float64(index)*278
		fmt.Fprintf(svg, `<rect x="%.1f" y="680" width="%.1f" height="88" rx="16" fill="%s" stroke="%s" stroke-width="1.5"/>`, x, width, palette.inner, palette.border)
		fmt.Fprintf(svg, `<rect x="%.1f" y="700" width="48" height="48" rx="13" fill="%s" opacity=".12"/>`, x+18, item.color)
		renderMetricIcon(svg, x+18, 700, item.icon, item.color)
		fmt.Fprintf(svg, `<text x="%.1f" y="720" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="25" font-weight="700" fill="%s">%s</text>`, x+78, palette.text, escapeXML(item.value))
		fmt.Fprintf(svg, `<text x="%.1f" y="748" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="17" fill="%s">%s</text>`, x+78, palette.secondary, escapeXML(item.label))
	}
}

type metricIcon string

const (
	metricBars     metricIcon = "bars"
	metricCalendar metricIcon = "calendar"
	metricStar     metricIcon = "star"
	metricGrowth   metricIcon = "growth"
)

// renderMetricIcon 使用 SVG 基元绘制指标图标，避免依赖 emoji/字体字形导致 README 渲染成方框。
func renderMetricIcon(svg *strings.Builder, x, y float64, icon metricIcon, color string) {
	switch icon {
	case metricBars:
		fmt.Fprintf(svg, `<rect x="%.1f" y="%.1f" width="7" height="14" rx="2" fill="%s"/><rect x="%.1f" y="%.1f" width="7" height="22" rx="2" fill="%s"/><rect x="%.1f" y="%.1f" width="7" height="30" rx="2" fill="%s"/>`, x+10, y+23, color, x+20, y+15, color, x+30, y+7, color)
	case metricCalendar:
		fmt.Fprintf(svg, `<rect x="%.1f" y="%.1f" width="28" height="25" rx="4" fill="none" stroke="%s" stroke-width="2"/><line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="2"/>`, x+10, y+11, color, x+10, y+19, x+38, y+19, color)
		for row := 0; row < 2; row++ {
			for column := 0; column < 3; column++ {
				fmt.Fprintf(svg, `<rect x="%.1f" y="%.1f" width="3" height="3" rx="1" fill="%s"/>`, x+16+float64(column)*8, y+24+float64(row)*7, color)
			}
		}
	case metricStar:
		renderStar(svg, x+24, y+24, 14, color)
	case metricGrowth:
		fmt.Fprintf(svg, `<path d="M%.1f %.1f L%.1f %.1f M%.1f %.1f H%.1f V%.1f" fill="none" stroke="%s" stroke-width="3.5" stroke-linecap="round" stroke-linejoin="round"/>`, x+11, y+34, x+37, y+8, x+23, y+8, x+37, y+22, color)
	}
}

func renderJourney(svg *strings.Builder, journey starJourney, locale Locale, palette palette) {
	labels := labelsFor(locale)
	fmt.Fprintf(svg, `<text x="54" y="824" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="21" font-weight="600" fill="%s">%s</text>`, palette.secondary, escapeXML(labels.journey))
	if len(journey.rankedEvents) == 0 {
		return
	}
	left, right := 62.0, 1118.0
	trackY := 858.0
	fmt.Fprintf(svg, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1.5"/>`, left, trackY, right, trackY, palette.grid)
	columns := len(journey.rankedEvents)
	for index, event := range journey.rankedEvents {
		if event.kind == journeyCurrent {
			fmt.Fprintf(svg, `<circle cx="%.1f" cy="%.1f" r="9" fill="%s" stroke="%s" stroke-width="3"/>`, right, trackY, palette.panel, palette.accent)
			continue
		}
		position := left + (right-left)*float64(index)/float64(maxInt(1, columns-1))
		color := palette.accent
		if event.kind == journeyCreated {
			color = palette.secondary
		} else if event.kind == journeyFirstRecorded {
			color = palette.green
		} else if event.kind == journeyBestDay || event.kind == journeyBestWeek || event.kind == journeySpike {
			color = palette.growth
		}
		fmt.Fprintf(svg, `<circle cx="%.1f" cy="%.1f" r="7" fill="%s" stroke="%s" stroke-width="2"/>`, position, trackY, color, palette.panel)
		fmt.Fprintf(svg, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="928" stroke="%s" stroke-width="1.5" stroke-dasharray="4 5"/>`, position, trackY+7, position, color)
		labelX := position + 14
		// 日期和备注与虚线同处 Journey 内容区，明确呈现为“虚线右侧”的说明，而不是下方居中标签。
		fmt.Fprintf(svg, `<text x="%.1f" y="892" text-anchor="start" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="16" fill="%s">%s</text>`, labelX, palette.secondary, escapeXML(formatDate(event.eventDate, locale, false)))
		fmt.Fprintf(svg, `<text x="%.1f" y="920" text-anchor="start" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="18" font-weight="600" fill="%s">%s</text>`, labelX, palette.text, escapeXML(eventTitle(event, locale)))
	}
}

func renderFooter(svg *strings.Builder, updated string, labels cardLabels, palette palette) {
	fmt.Fprintf(svg, `<line x1="54" y1="950" x2="1146" y2="950" stroke="%s" stroke-width="1.5"/>`, palette.border)
	fmt.Fprintf(svg, `<circle cx="68" cy="995" r="14" fill="none" stroke="%s" stroke-width="2"/><line x1="68" y1="995" x2="68" y2="986" stroke="%s" stroke-width="2"/><line x1="68" y1="995" x2="75" y2="995" stroke="%s" stroke-width="2"/>`, palette.secondary, palette.secondary, palette.secondary)
	fmt.Fprintf(svg, `<text x="94" y="1002" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="18" fill="%s">%s %s</text>`, palette.secondary, escapeXML(labels.updated), escapeXML(updated))
	fmt.Fprintf(svg, `<text x="1146" y="1002" text-anchor="end" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" font-size="18" fill="%s">✦ %s <tspan fill="%s" font-weight="700">Starcat</tspan></text>`, palette.secondary, escapeXML(labels.poweredBy), palette.brand)
}

type historyMetrics struct {
	ageDays      *int
	growth       *int
	dailyAverage *float64
	growthRate   *float64
	sinceCreated bool
	periodDays   *int
}

// buildMetrics 与 Starcat ReadmeStarHistoryMetrics 保持同一 90 天窗口和覆盖门禁。
// 创建日补点不进入 points，因此不会把视觉基线误当成真实历史快照。
func buildMetrics(points []model.HistoryPoint, days []int, createdAt, coverageStart, now time.Time) historyMetrics {
	result := historyMetrics{}
	if !createdAt.IsZero() && !now.IsZero() && !createdAt.After(now) {
		days := calendarDays(createdAt, now)
		result.ageDays = &days
	}
	latest := points[len(points)-1]
	latestDate := parseDate(latest.Date)
	cutoff := latestDate.Add(-90 * 24 * time.Hour)
	recentCreation := !createdAt.IsZero() && createdAt.After(cutoff) && !createdAt.After(latestDate)
	result.sinceCreated = recentCreation
	start := cutoff
	if recentCreation {
		start = createdAt
	}
	periodDays := calendarDays(start, latestDate)
	result.periodDays = &periodDays
	baseline := lastPointAtOrBefore(points, days, cutoff)
	baselineCount := 0
	if !recentCreation {
		if baseline == nil {
			return result
		}
		baselineCount = baseline.Count
	}
	if !start.Before(latestDate) || (!coverageStart.IsZero() && coverageStart.After(cutoff) && !recentCreation) {
		return result
	}
	growth := latest.Count - baselineCount
	result.growth = &growth
	if baselineCount > 0 {
		rate := float64(growth) / float64(baselineCount)
		result.growthRate = &rate
	}
	if periodDays > 0 {
		average := float64(growth) / float64(periodDays)
		result.dailyAverage = &average
	}
	return result
}

type journeyKind string

const (
	journeyCreated       journeyKind = "created"
	journeyFirstRecorded journeyKind = "firstRecorded"
	journeyMilestone     journeyKind = "milestone"
	journeyBestDay       journeyKind = "bestDay"
	journeyBestWeek      journeyKind = "bestWeek"
	journeySpike         journeyKind = "spike"
	journeyCurrent       journeyKind = "current"
)

type journeyEvent struct {
	kind                 journeyKind
	eventDate, pointDate time.Time
	point                *model.HistoryPoint
	threshold, growth    int
	previous             *model.HistoryPoint
	priority             float64
}

type starJourney struct {
	rankedEvents []journeyEvent
	chartEvents  []journeyEvent
}

func buildJourney(points []model.HistoryPoint, days []int, createdAt time.Time, current int, coverageStart time.Time) starJourney {
	latest := &points[len(points)-1]
	latestDate := parseDate(latest.Date)
	currentEvent := journeyEvent{kind: journeyCurrent, eventDate: latestDate, pointDate: latestDate, point: latest, priority: 1000}
	events := make([]journeyEvent, 0, 8)
	if !createdAt.IsZero() && dayNumber(createdAt) <= dayNumber(currentEvent.eventDate) {
		events = append(events, journeyEvent{kind: journeyCreated, eventDate: createdAt, priority: 900})
	}
	if current > 0 {
		for index, point := range points {
			if isHistorySource(point.Source) && point.Count > 0 && index != len(points)-1 {
				date := parseDate(point.Date)
				events = append(events, journeyEvent{kind: journeyFirstRecorded, eventDate: date, pointDate: date, point: &points[index], priority: 76})
				break
			}
		}
	}
	for _, threshold := range thresholds(current) {
		if points[0].Count >= threshold {
			continue
		}
		for index := 1; index < len(points); index++ {
			if points[index-1].Count < threshold && points[index].Count >= threshold {
				if index == len(points)-1 {
					currentEvent.threshold = threshold
					currentEvent.previous = &points[index-1]
				} else {
					date := parseDate(points[index].Date)
					events = append(events, journeyEvent{kind: journeyMilestone, eventDate: date, pointDate: date, point: &points[index], threshold: threshold, previous: &points[index-1], priority: milestonePriority(threshold)})
				}
				break
			}
		}
	}
	if growth := growthEvent(points, days, current, coverageStart); growth != nil {
		events = append(events, *growth)
	}
	events = append(events, currentEvent)
	ranked := rankJourney(events)
	if len(ranked) > 7 {
		ranked = ranked[:7]
	}
	chartCandidates := make([]journeyEvent, 0, 4)
	for _, event := range events {
		if event.kind == journeyCurrent || event.kind == journeyMilestone || event.kind == journeySpike || (current < 10 && event.kind == journeyFirstRecorded && event.point != nil && event.point.Count > 0) {
			chartCandidates = append(chartCandidates, event)
		}
	}
	chart := rankJourney(chartCandidates)
	if len(chart) > 4 {
		chart = chart[:4]
	}
	return starJourney{rankedEvents: ranked, chartEvents: chart}
}

func thresholds(current int) []int {
	switch {
	case current < 10:
		return []int{1, 3, 5, 10}
	case current < 100:
		return []int{10, 25, 50, 100}
	case current < 1000:
		return []int{10, 100, 250, 500, 1000}
	case current < 10000:
		return []int{1000, 2500, 5000, 10000}
	case current < 100000:
		return []int{1000, 10000, 25000, 50000, 100000}
	default:
		scale := math.Pow(10, math.Floor(math.Log10(float64(current))))
		result := make([]int, 0, 5)
		for _, multiplier := range []float64{0.1, 1, 2.5, 5, 10} {
			value := scale * multiplier
			if value < float64(math.MaxInt) {
				result = append(result, int(value))
			}
		}
		return result
	}
}

func milestonePriority(threshold int) float64 {
	value := math.Max(1, float64(threshold))
	if math.Abs(math.Log10(value)-math.Round(math.Log10(value))) < 0.0001 {
		return 82
	}
	return 58
}

func growthEvent(points []model.HistoryPoint, days []int, total int, coverageStart time.Time) *journeyEvent {
	if len(points) < 2 {
		return nil
	}
	dayMinimum := maxInt(3, int(math.Ceil(float64(total)*0.01)))
	weekMinimum := maxInt(5, int(math.Ceil(float64(total)*0.02)))
	spikeMinimum := maxInt(10, int(math.Ceil(float64(total)*0.05)))
	var bestDay, bestWeek, spike *journeyEvent
	for index := 1; index < len(points); index++ {
		previous, point := points[index-1], points[index]
		if !isHistorySource(previous.Source) || !isHistorySource(point.Source) || previous.Precision != point.Precision {
			continue
		}
		if !coverageStart.IsZero() && parseDate(previous.Date).Before(coverageStart) {
			continue
		}
		endDay := days[index]
		daily := point.Count - previous.Count
		if daily >= dayMinimum && (bestDay == nil || daily > bestDay.growth) {
			date := dateFromDay(days[index])
			candidate := journeyEvent{kind: journeyBestDay, eventDate: date, pointDate: date, point: &points[index], growth: daily, priority: 68}
			bestDay = &candidate
		}
		baseline, ok := valueAtOrBefore(points, days, endDay-7)
		if !ok {
			continue
		}
		weekly := point.Count - baseline.Count
		if weekly >= weekMinimum && (bestWeek == nil || weekly > bestWeek.growth) {
			date := dateFromDay(days[index])
			candidate := journeyEvent{kind: journeyBestWeek, eventDate: date, pointDate: date, point: &points[index], growth: weekly, priority: 72}
			bestWeek = &candidate
		}
		if earlier, ok := valueAtOrBefore(points, days, endDay-35); ok && weekly >= spikeMinimum && float64(weekly) >= float64(maxInt(0, baseline.Count-earlier.Count))/4*3 && (spike == nil || weekly > spike.growth) {
			date := dateFromDay(days[index])
			candidate := journeyEvent{kind: journeySpike, eventDate: date, pointDate: date, point: &points[index], growth: weekly, priority: 100}
			spike = &candidate
		}
	}
	if spike != nil {
		return spike
	}
	if bestWeek != nil && (bestDay == nil || float64(bestWeek.growth) >= float64(bestDay.growth)*1.5) {
		return bestWeek
	}
	return bestDay
}

// isHistorySource 统一识别旧的归档数据和 GitHub 官方历史接口数据。
// 两者都属于完整历史快照，可参与 Starcat Journey；不能只检查旧的 gh_archive，
// 否则切换官方 API 后首次记录和增长事件会悄悄消失。
func isHistorySource(source string) bool {
	return source == "gh_archive" || source == "github_history"
}

func rankJourney(events []journeyEvent) []journeyEvent {
	remaining := append([]journeyEvent(nil), events...)
	selected := make([]journeyEvent, 0, len(events))
	var minDate, maxDate time.Time
	for _, event := range events {
		if event.eventDate.IsZero() {
			continue
		}
		if minDate.IsZero() || event.eventDate.Before(minDate) {
			minDate = event.eventDate
		}
		if maxDate.IsZero() || event.eventDate.After(maxDate) {
			maxDate = event.eventDate
		}
	}
	duration := math.Max(1, maxDate.Sub(minDate).Seconds())
	for len(remaining) > 0 {
		bestIndex := 0
		bestScore := math.Inf(-1)
		for index, event := range remaining {
			score := event.priority
			if len(selected) > 0 && !event.eventDate.IsZero() {
				nearest := math.Inf(1)
				for _, chosen := range selected {
					nearest = math.Min(nearest, math.Abs(event.eventDate.Sub(chosen.eventDate).Seconds()))
				}
				score += math.Min(1, nearest/duration) * 32
			}
			if score > bestScore {
				bestScore, bestIndex = score, index
			}
		}
		selected = append(selected, remaining[bestIndex])
		remaining = append(remaining[:bestIndex], remaining[bestIndex+1:]...)
	}
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].eventDate.Before(selected[j].eventDate) })
	return selected
}

func eventTitle(event journeyEvent, locale Locale) string {
	if locale == LocaleChinese {
		switch event.kind {
		case journeyCreated:
			return "仓库创建"
		case journeyFirstRecorded:
			return "首次记录"
		case journeyMilestone:
			return "突破 " + compactNumber(event.threshold) + " Stars"
		case journeyBestDay:
			return "单日增长"
		case journeyBestWeek:
			return "单周增长"
		case journeySpike:
			return "增长 Spike"
		}
		return "当前 Stars"
	}
	switch event.kind {
	case journeyCreated:
		return "Repository Created"
	case journeyFirstRecorded:
		return "First Recorded"
	case journeyMilestone:
		return "Reached " + compactNumber(event.threshold) + " Stars"
	case journeyBestDay:
		return "Best Day"
	case journeyBestWeek:
		return "Best Week"
	case journeySpike:
		return "Growth Spike"
	}
	return "Current Stars"
}

func addingCreationBaseline(points []model.HistoryPoint, createdAt time.Time) []model.HistoryPoint {
	if createdAt.IsZero() || len(points) == 0 {
		return append([]model.HistoryPoint(nil), points...)
	}
	first := parseDate(points[0].Date)
	if dayNumber(createdAt) == dayNumber(first) {
		baseline := points[0]
		baseline.Date = createdAt.Format("2006-01-02")
		return append([]model.HistoryPoint{baseline}, points[1:]...)
	}
	if dayNumber(createdAt) > dayNumber(first) {
		return append([]model.HistoryPoint(nil), points...)
	}
	baseline := model.HistoryPoint{Date: createdAt.Format("2006-01-02"), Count: 0, Source: points[0].Source, Precision: "estimated"}
	return append([]model.HistoryPoint{baseline}, points...)
}

func renderedPointsWithAnchors(points []model.HistoryPoint, events []journeyEvent) []model.HistoryPoint {
	anchors := make(map[string]model.HistoryPoint)
	for _, event := range events {
		if event.point != nil {
			anchors[event.point.Date] = *event.point
		}
	}
	base := largestTriangleThreeBuckets(points, maximumDrawnPoints)
	known := make(map[string]struct{}, len(base))
	for _, point := range base {
		known[point.Date] = struct{}{}
	}
	missing := make([]model.HistoryPoint, 0, len(anchors))
	for date, point := range anchors {
		if _, ok := known[date]; !ok {
			missing = append(missing, point)
		}
	}
	if len(base)+len(missing) > maximumDrawnPoints {
		base = largestTriangleThreeBuckets(points, maximumDrawnPoints-len(anchors))
		known = make(map[string]struct{}, len(base))
		for _, point := range base {
			known[point.Date] = struct{}{}
		}
		missing = missing[:0]
		for date, point := range anchors {
			if _, ok := known[date]; !ok {
				missing = append(missing, point)
			}
		}
	}
	result := append(base, missing...)
	sort.SliceStable(result, func(i, j int) bool { return result[i].Date < result[j].Date })
	return result
}

// largestTriangleThreeBuckets 保持与 Starcat 相同的 LTTB 语义，保留首尾和视觉突变。
func largestTriangleThreeBuckets(points []model.HistoryPoint, threshold int) []model.HistoryPoint {
	if threshold >= len(points) || threshold < 3 {
		return append([]model.HistoryPoint(nil), points...)
	}
	result := []model.HistoryPoint{points[0]}
	bucketWidth := float64(len(points)-2) / float64(threshold-2)
	previousIndex := 0
	for bucketIndex := 0; bucketIndex < threshold-2; bucketIndex++ {
		averageStart := minInt(int(math.Floor(float64(bucketIndex+1)*bucketWidth))+1, len(points)-1)
		averageEnd := minInt(int(math.Floor(float64(bucketIndex+2)*bucketWidth))+1, len(points))
		if averageEnd <= averageStart {
			averageEnd = minInt(averageStart+1, len(points))
		}
		averageX, averageY := 0.0, 0.0
		for index := averageStart; index < averageEnd; index++ {
			averageX += parseDate(points[index].Date).Sub(parseDate(points[0].Date)).Hours()
			averageY += float64(points[index].Count)
		}
		count := float64(maxInt(1, averageEnd-averageStart))
		averageX, averageY = averageX/count, averageY/count
		candidateStart := minInt(int(math.Floor(float64(bucketIndex)*bucketWidth))+1, len(points)-2)
		candidateEnd := minInt(int(math.Floor(float64(bucketIndex+1)*bucketWidth))+1, len(points)-1)
		if candidateEnd <= candidateStart {
			candidateEnd = candidateStart + 1
		}
		previousX := parseDate(points[previousIndex].Date).Sub(parseDate(points[0].Date)).Hours()
		previousY := float64(points[previousIndex].Count)
		selected, largestArea := candidateStart, -1.0
		for index := candidateStart; index < candidateEnd; index++ {
			candidateX := parseDate(points[index].Date).Sub(parseDate(points[0].Date)).Hours()
			candidateY := float64(points[index].Count)
			area := math.Abs((previousX-averageX)*(candidateY-previousY) - (previousX-candidateX)*(averageY-previousY))
			if area > largestArea {
				selected, largestArea = index, area
			}
		}
		result = append(result, points[selected])
		previousIndex = selected
	}
	return append(result, points[len(points)-1])
}

type coordinate struct{ x, y float64 }

func coordinateText(points []coordinate) string {
	parts := make([]string, 0, len(points))
	for _, point := range points {
		parts = append(parts, fmt.Sprintf("%.2f,%.2f", point.x, point.y))
	}
	return strings.Join(parts, " ")
}

func preparePoints(points []model.HistoryPoint) ([]model.HistoryPoint, error) {
	result := append([]model.HistoryPoint(nil), points...)
	sort.SliceStable(result, func(i, j int) bool { return result[i].Date < result[j].Date })
	for index, point := range result {
		if point.Count < 0 || parseDate(point.Date).IsZero() {
			return nil, fmt.Errorf("invalid history point at index %d", index)
		}
		if index > 0 && result[index-1].Date == point.Date {
			return nil, fmt.Errorf("duplicate history date %q", point.Date)
		}
	}
	return result, nil
}

// parseDate 解析 "YYYY-MM-DD"（服务端唯一会生成的日期格式）。
//
// 不用 time.Parse：它每次都要走一遍格式扫描（实测解析一个日期约 0.6µs），而渲染
// 长历史时这个函数会被调用几十万次（growthEvent 对每个点都要做两次"某天之前最近的
// 点"查询），golang/go 这种 600+ 周的历史因此要 1 秒 CPU。手写拆分把单次成本降到
// 几十纳秒，并保留 time.Parse 兜底以兼容非预期输入（例如测试夹具里的其它格式）。
func parseDate(raw string) time.Time {
	if len(raw) == 10 && raw[4] == '-' && raw[7] == '-' {
		year, errYear := leadingDigits(raw[0:4])
		month, errMonth := leadingDigits(raw[5:7])
		day, errDay := leadingDigits(raw[8:10])
		if errYear == nil && errMonth == nil && errDay == nil {
			value := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
			// 必须回读校验：time.Date 会把 2026-02-30、2026-13-01 这类越界日期
			// 规范化成另一天，而 time.Parse 会直接失败。上层靠"零值 = 非法日期"
			// 拒绝脏数据，这里要保持同样的严格性。
			if value.Year() == year && int(value.Month()) == month && value.Day() == day {
				return value
			}
			return time.Time{}
		}
	}
	value, _ := time.Parse("2006-01-02", raw)
	return value
}

// leadingDigits 解析定长的十进制片段；出现非数字即失败。
func leadingDigits(raw string) (int, error) {
	value := 0
	for index := 0; index < len(raw); index++ {
		character := raw[index]
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("invalid date digit %q", character)
		}
		value = value*10 + int(character-'0')
	}
	return value, nil
}

// pointDays 预计算每个点的日序号，供按日查询使用。
//
// points 在 preparePoints 之后按日期升序，因此可以配合二分查找把"某天之前最近的点"
// 从 O(n) 降到 O(log n)：这个查询在 growthEvent 里是逐点调用的，累计起来是渲染长
// 历史时最大的开销来源。
func pointDays(points []model.HistoryPoint) []int {
	days := make([]int, len(points))
	for index, point := range points {
		days[index] = dayNumber(parseDate(point.Date))
	}
	return days
}

// dateFromDay 是 dayNumber 的逆运算：把日序号还原为 UTC 零点。
func dateFromDay(day int) time.Time { return time.Unix(int64(day)*86_400, 0).UTC() }

func lastPointAtOrBefore(points []model.HistoryPoint, days []int, target time.Time) *model.HistoryPoint {
	point, ok := valueAtOrBefore(points, days, dayNumber(target))
	if !ok {
		return nil
	}
	return point
}

// valueAtOrBefore 返回日序号 <= day 的最后一个点；points 与 days 必须等长且按日升序。
func valueAtOrBefore(points []model.HistoryPoint, days []int, day int) (*model.HistoryPoint, bool) {
	if len(points) != len(days) {
		return nil, false
	}
	index := sort.SearchInts(days, day+1) - 1
	if index < 0 || index >= len(points) {
		return nil, false
	}
	return &points[index], true
}

func maximumCount(points []model.HistoryPoint) int {
	maximum := 0
	for _, point := range points {
		maximum = maxInt(maximum, point.Count)
	}
	return maximum
}

func dayNumber(value time.Time) int { return int(math.Floor(float64(value.Unix()) / 86_400)) }

func calendarDays(start, end time.Time) int {
	start = time.Date(start.UTC().Year(), start.UTC().Month(), start.UTC().Day(), 0, 0, 0, 0, time.UTC)
	end = time.Date(end.UTC().Year(), end.UTC().Month(), end.UTC().Day(), 0, 0, 0, 0, time.UTC)
	return int(end.Sub(start).Hours() / 24)
}

func formatDailyAverage(value *float64) string {
	if value == nil {
		return "—"
	}
	number := *value
	if math.Abs(number) >= 1000 {
		return signedPrefix(number) + compactNumber(int(math.Abs(number)))
	}
	precision := 1
	if math.Abs(number) < 1 {
		precision = 2
	}
	return signedPrefix(number) + strconv.FormatFloat(math.Abs(number), 'f', precision, 64)
}

func formatAge(value *int, locale Locale) string {
	if value == nil {
		return "—"
	}
	if locale == LocaleChinese {
		return strconv.Itoa(*value) + "天"
	}
	return strconv.Itoa(*value) + " days"
}

func formatSigned(value *int) string {
	if value == nil {
		return "—"
	}
	return signedPrefix(float64(*value)) + compactNumber(int(math.Abs(float64(*value))))
}

func formatRate(value *float64) string {
	if value == nil {
		return "—"
	}
	percent := *value * 100
	if math.Abs(percent) >= 1000 {
		return signedPrefix(percent) + compactNumber(int(math.Abs(percent))) + "%"
	}
	precision := 0
	if math.Abs(percent) < 100 {
		precision = 1
	}
	return signedPrefix(percent) + strconv.FormatFloat(math.Abs(percent), 'f', precision, 64) + "%"
}

func signedPrefix(value float64) string {
	if value > 0 {
		return "+"
	}
	if value < 0 {
		return "-"
	}
	return ""
}

func compactNumber(value int) string {
	if value < 1000 {
		return strconv.Itoa(value)
	}
	units := []string{"", "K", "M", "B"}
	number := float64(value)
	unit := 0
	for number >= 1000 && unit < len(units)-1 {
		number /= 1000
		unit++
	}
	result := strconv.FormatFloat(number, 'f', 1, 64)
	result = strings.TrimSuffix(strings.TrimSuffix(result, "0"), ".")
	return result + units[unit]
}

func formatAxisDate(value time.Time, duration time.Duration, locale Locale) string {
	if locale == LocaleChinese {
		if duration <= 180*24*time.Hour {
			return fmt.Sprintf("%d月%d日", int(value.Month()), value.Day())
		}
		if duration/5 >= 365*24*time.Hour {
			return strconv.Itoa(value.Year())
		}
		return fmt.Sprintf("%d年%02d月", value.Year(), int(value.Month()))
	}
	if duration/5 >= 365*24*time.Hour {
		return strconv.Itoa(value.Year())
	}
	return value.Format("Jan 2006")
}

func formatDate(value time.Time, locale Locale, includeYear bool) string {
	if locale == LocaleChinese {
		if includeYear {
			return fmt.Sprintf("%d年%d月%d日", value.Year(), int(value.Month()), value.Day())
		}
		return fmt.Sprintf("%d月%d日", int(value.Month()), value.Day())
	}
	if includeYear {
		return value.Format("Jan 2, 2006")
	}
	return value.Format("Jan 2")
}

func accessibilityDescription(fullName string, stars int, locale Locale) string {
	if locale == LocaleChinese {
		return fmt.Sprintf("%s 的 Star 历史，当前 %s Stars。", fullName, compactNumber(stars))
	}
	return fmt.Sprintf("Star history for %s, currently %s Stars.", fullName, compactNumber(stars))
}

func ownerInitial(fullName string) string {
	owner := fullName
	if slash := strings.IndexByte(owner, '/'); slash >= 0 {
		owner = owner[:slash]
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return "S"
	}
	return strings.ToUpper(string([]rune(owner)[0]))
}

func truncateText(value string, maximum int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maxInt(0, maximum-1)]) + "…"
}

func renderStar(svg *strings.Builder, cx, cy, radius float64, color string) {
	points := make([]string, 0, 10)
	for index := 0; index < 10; index++ {
		angle := -math.Pi/2 + float64(index)*math.Pi/5
		currentRadius := radius
		if index%2 == 1 {
			currentRadius *= 0.45
		}
		points = append(points, fmt.Sprintf("%.2f,%.2f", cx+math.Cos(angle)*currentRadius, cy+math.Sin(angle)*currentRadius))
	}
	fmt.Fprintf(svg, `<polygon points="%s" fill="%s"/>`, strings.Join(points, " "), color)
}

func safeAvatarDataURI(value string) string {
	for _, mimeType := range []string{"image/png", "image/jpeg", "image/webp", "image/gif"} {
		prefix := "data:" + mimeType + ";base64,"
		if !strings.HasPrefix(value, prefix) {
			continue
		}
		encoded := strings.TrimPrefix(value, prefix)
		if encoded == "" {
			return ""
		}
		if _, err := base64.StdEncoding.DecodeString(encoded); err == nil {
			return value
		}
	}
	return ""
}

func escapeXML(value string) string { return html.EscapeString(value) }

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
