package card

import (
	"encoding/xml"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/model"
)

func testRenderInput() RenderInput {
	return RenderInput{
		FullName:     "owner/repo",
		CurrentStars: 100,
		Points: []model.HistoryPoint{
			{Date: "2026-01-01", Count: 10},
			{Date: "2026-02-01", Count: 20},
			{Date: "2026-03-01", Count: 100},
		},
		GeneratedAt: time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		Theme:       ThemeLight,
		Locale:      LocaleEnglish,
	}
}

func TestRenderIsDeterministicAndSelfContained(t *testing.T) {
	input := testRenderInput()
	first, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("same render input must produce identical SVG")
	}
	value := string(first)
	for _, forbidden := range []string{"<script", "href=\"http", "src=\"http", "<style"} {
		if strings.Contains(value, forbidden) {
			t.Fatalf("SVG must be self-contained, found %q", forbidden)
		}
	}
	if !strings.Contains(value, `xmlns="http://www.w3.org/2000/svg"`) {
		t.Fatal("missing SVG namespace")
	}
	if err := xml.Unmarshal(first, &struct{}{}); err != nil {
		t.Fatalf("SVG must be well-formed XML: %v", err)
	}
}

func TestRenderEscapesRepositoryText(t *testing.T) {
	input := testRenderInput()
	input.FullName = `<owner>/repo & "demo"`
	content, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	value := string(content)
	if !strings.Contains(value, "&lt;owner&gt;/repo &amp; &#34;demo&#34;") {
		t.Fatalf("repository text was not XML escaped: %s", value)
	}
	if strings.Contains(value, `<owner>`) {
		t.Fatal("raw repository markup must not appear in SVG")
	}
}

func TestRenderSupportsThemeAndLocale(t *testing.T) {
	input := testRenderInput()
	input.Theme = ThemeDark
	input.Locale = LocaleChinese
	content, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	value := string(content)
	if !strings.Contains(value, `fill="#0e131b"`) || !strings.Contains(value, "GitHub Star History") || !strings.Contains(value, "总 Stars") || strings.Contains(value, "当前 Stars") {
		t.Fatalf("dark Chinese SVG did not use requested presentation: %s", value)
	}
}

func TestRenderIncludesStarcatCardSections(t *testing.T) {
	input := testRenderInput()
	input.Locale = LocaleChinese
	input.Points = append([]model.HistoryPoint{{Date: "2025-12-01", Count: 5}}, input.Points...)
	input.Description = "A useful repository description"
	input.Language = "Go"
	input.Topics = []string{"github", "history"}
	input.AvatarDataURI = "data:image/png;base64,AAAA"
	input.CreatedAt = time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	input.CoverageStart = time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	content, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	value := string(content)
	for _, marker := range []string{
		`viewBox="0 0 1200 1060"`, "A useful repository description", "Go", "日均新增", "Star Journey", "Powered by", "<image ", `y="892" text-anchor="start"`, `y="920" text-anchor="start"`,
		"linearGradient", "polygon", "polyline",
	} {
		if !strings.Contains(value, marker) {
			t.Fatalf("Starcat card section %q is missing: %s", marker, value)
		}
	}
}

func TestRenderDoesNotAddCalloutToCurrentPoint(t *testing.T) {
	content, err := Render(testRenderInput())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), `width="144.0" height="60.0"`) {
		t.Fatal("current point must keep its endpoint marker without a curve callout")
	}
	if !strings.Contains(string(content), `r="10" fill="none"`) {
		t.Fatal("current point endpoint ring is missing")
	}
}

func TestMetricsUseTheSameNinetyDayWindow(t *testing.T) {
	points := []model.HistoryPoint{
		{Date: "2025-12-31", Count: 100, Source: "gh_archive", Precision: "estimated"},
		{Date: "2026-01-31", Count: 125, Source: "gh_archive", Precision: "estimated"},
		{Date: "2026-03-31", Count: 200, Source: "gh_archive", Precision: "estimated"},
	}
	coverageStart := time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)
	metrics := buildMetrics(points, time.Time{}, coverageStart, time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC))
	if metrics.growth == nil || *metrics.growth != 100 {
		t.Fatalf("unexpected ninety-day growth: %#v", metrics.growth)
	}
	if metrics.dailyAverage == nil || math.Abs(*metrics.dailyAverage-100.0/90.0) > 0.0001 {
		t.Fatalf("unexpected daily average: %#v", metrics.dailyAverage)
	}
	if metrics.growthRate == nil || *metrics.growthRate != 1 {
		t.Fatalf("unexpected growth rate: %#v", metrics.growthRate)
	}
}

func TestMetricsUseZeroBaselineForRecentlyCreatedRepository(t *testing.T) {
	points := []model.HistoryPoint{
		{Date: "2026-03-05", Count: 10, Source: "gh_archive", Precision: "estimated"},
		{Date: "2026-03-10", Count: 30, Source: "gh_archive", Precision: "estimated"},
	}
	metrics := buildMetrics(points, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Time{}, time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC))
	if metrics.growth == nil || *metrics.growth != 30 || metrics.growthRate != nil {
		t.Fatalf("recent repository metrics must use zero baseline: %#v", metrics)
	}
}

func TestLargestTriangleThreeBucketsKeepsEndpointsAndSpike(t *testing.T) {
	points := make([]model.HistoryPoint, 0, 12)
	for index := 0; index < 12; index++ {
		count := 10
		if index == 6 {
			count = 1_000
		}
		points = append(points, model.HistoryPoint{Date: time.Date(2026, 1, index+1, 0, 0, 0, 0, time.UTC).Format("2006-01-02"), Count: count})
	}
	selected := largestTriangleThreeBuckets(points, 5)
	if len(selected) != 5 || selected[0].Date != points[0].Date || selected[len(selected)-1].Date != points[len(points)-1].Date {
		t.Fatalf("LTTB must keep endpoints: %#v", selected)
	}
	foundSpike := false
	for _, point := range selected {
		foundSpike = foundSpike || point.Count == 1_000
	}
	if !foundSpike {
		t.Fatalf("LTTB must retain the dominant spike: %#v", selected)
	}
}

func TestCompactNumberUsesExpectedSuffix(t *testing.T) {
	tests := map[int]string{
		999:       "999",
		1_000:     "1K",
		319_700:   "319.7K",
		1_300_000: "1.3M",
	}
	for value, expected := range tests {
		if actual := compactNumber(value); actual != expected {
			t.Fatalf("compactNumber(%d) = %q, want %q", value, actual, expected)
		}
	}
}

func TestPlaceCalloutAvoidsOverlap(t *testing.T) {
	points := []coordinate{
		{x: 420, y: 310},
		{x: 420, y: 330},
		{x: 420, y: 350},
	}
	occupied := make([]calloutPlacement, 0, len(points))
	for _, point := range points {
		placement, ok := placeCallout(point, occupied)
		if !ok {
			t.Fatalf("expected a non-overlapping placement for point %#v after %#v", point, occupied)
		}
		if calloutOverlaps(placement, occupied) {
			t.Fatalf("placement %#v overlaps an existing callout", placement)
		}
		occupied = append(occupied, placement)
	}
}

func TestPlaceCalloutClampsToChartBounds(t *testing.T) {
	placement, ok := placeCallout(coordinate{x: chartRight, y: chartTop}, nil)
	if !ok {
		t.Fatal("expected a placement at the top-right chart boundary")
	}
	if placement.x < chartLeft+4 || placement.x+calloutWidth > chartRight-4 {
		t.Fatalf("callout escaped horizontal bounds: %#v", placement)
	}
	if placement.y < chartTop-calloutHeight-2 || placement.y+calloutHeight > chartBottom-2 {
		t.Fatalf("callout escaped vertical bounds: %#v", placement)
	}
}

func TestBuildJourneyRecognizesOfficialHistorySource(t *testing.T) {
	points := []model.HistoryPoint{
		{Date: "2026-01-01", Count: 0, Source: "github_history", Precision: "reconstructed"},
		{Date: "2026-01-02", Count: 20, Source: "github_history", Precision: "reconstructed"},
		{Date: "2026-01-03", Count: 100, Source: "github_history", Precision: "reconstructed"},
	}
	journey := buildJourney(points, time.Time{}, 100, time.Time{})
	foundFirstRecorded := false
	for _, event := range journey.rankedEvents {
		if event.kind == journeyFirstRecorded && event.point != nil && event.point.Count == 20 {
			foundFirstRecorded = true
		}
	}
	if !foundFirstRecorded {
		t.Fatalf("official history data should produce a first-recorded event: %#v", journey.rankedEvents)
	}
	if growth := growthEvent(points, 100, time.Time{}); growth == nil || growth.kind != journeyBestDay {
		t.Fatalf("official history data should participate in growth events: %#v", growth)
	}
}
