package series

import (
	"testing"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/model"
)

func TestNormalizeCalibratesLastPoint(t *testing.T) {
	events := []DayCount{{Day: 17_000, Count: 1}, {Day: 17_001, Count: 3}}
	points, err := Normalize(events, 100)
	if err != nil {
		t.Fatal(err)
	}
	if points[0].Count != 25 || points[1].Count != 100 {
		t.Fatalf("unexpected calibrated points: %#v", points)
	}
	if points[0].Source != "gh_archive" || points[0].Precision != "estimated" {
		t.Fatalf("unexpected provenance: %#v", points[0])
	}
}

func TestSelectRangeDownsamplesAllByMonth(t *testing.T) {
	points := []model.HistoryPoint{
		{Date: "2026-01-01", Count: 1},
		{Date: "2026-01-31", Count: 2},
		{Date: "2026-02-01", Count: 3},
	}
	selected, err := SelectRange(points, model.HistoryRangeAll, time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC), 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || selected[0].Count != 2 || selected[1].Count != 3 {
		t.Fatalf("unexpected selected points: %#v", selected)
	}
}
