package series

import (
	"errors"
	"testing"
	"time"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	points := []DayCount{{Day: DayFromTime(time.Date(2016, 1, 1, 0, 0, 0, 0, time.UTC)), Count: 3}, {Day: DayFromTime(time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)), Count: 9}}
	payload, checksum, err := Encode(points)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(Encoding, payload, checksum, len(points))
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 || decoded[1] != points[1] {
		t.Fatalf("unexpected decoded points: %#v", decoded)
	}
}

func TestDecodeRejectsChecksumMismatch(t *testing.T) {
	payload, _, err := Encode([]DayCount{{Day: 17_000, Count: 1}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Decode(Encoding, payload, "bad", 1)
	if !errors.Is(err, ErrCorruptSeries) {
		t.Fatalf("expected ErrCorruptSeries, got %v", err)
	}
}
