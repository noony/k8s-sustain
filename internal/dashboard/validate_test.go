package dashboard

import (
	"net/url"
	"testing"
	"time"
)

func TestParseTimeRangeAbsolute(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	q := url.Values{"from": {"1718000000"}, "to": {"1718003600"}}
	tr, perr := parseTimeRange(q, "168h", now)
	if perr != nil {
		t.Fatalf("unexpected error: %+v", perr)
	}
	if tr.Start.Unix() != 1718000000 || tr.End.Unix() != 1718003600 {
		t.Errorf("got [%d,%d]", tr.Start.Unix(), tr.End.Unix())
	}
}

func TestParseTimeRangeFallsBackToWindow(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	tr, perr := parseTimeRange(url.Values{}, "1h", now)
	if perr != nil {
		t.Fatalf("unexpected error: %+v", perr)
	}
	if !tr.End.Equal(now) || !tr.Start.Equal(now.Add(-time.Hour)) {
		t.Errorf("fallback wrong: [%v,%v]", tr.Start, tr.End)
	}
}

func TestParseTimeRangeInvalid(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	cases := map[string]url.Values{
		"missing to":    {"from": {"1718000000"}},
		"non-numeric":   {"from": {"x"}, "to": {"1718003600"}},
		"from after to": {"from": {"1718003600"}, "to": {"1718000000"}},
	}
	for name, q := range cases {
		if _, perr := parseTimeRange(q, "1h", now); perr == nil {
			t.Errorf("%s: expected paramError", name)
		}
	}
}
