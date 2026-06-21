package prometheus

import (
	"testing"
	"time"
)

func TestTimeRangeFromWindow(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)

	tr, err := TimeRangeFromWindow("1h", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !tr.End.Equal(now) {
		t.Errorf("End = %v, want %v", tr.End, now)
	}
	if want := now.Add(-time.Hour); !tr.Start.Equal(want) {
		t.Errorf("Start = %v, want %v", tr.Start, want)
	}
}

func TestTimeRangeFromWindowInvalid(t *testing.T) {
	if _, err := TimeRangeFromWindow("bogus", time.Now()); err == nil {
		t.Fatal("expected error for invalid window")
	}
}
