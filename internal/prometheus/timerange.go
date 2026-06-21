package prometheus

import (
	"fmt"
	"time"

	"github.com/prometheus/common/model"
)

// TimeRange is an explicit, absolute query window. Resolving "what time is it"
// happens once, at the HTTP edge, so the range methods below stay deterministic
// and easy to test.
type TimeRange struct {
	Start time.Time
	End   time.Time
}

// TimeRangeFromWindow builds a trailing range ending at now: [now-window, now].
// window uses Prometheus duration syntax (e.g. "1h", "7d").
func TimeRangeFromWindow(window string, now time.Time) (TimeRange, error) {
	d, err := model.ParseDuration(window)
	if err != nil {
		return TimeRange{}, fmt.Errorf("parse window %q: %w", window, err)
	}
	return TimeRange{Start: now.Add(-time.Duration(d)), End: now}, nil
}
