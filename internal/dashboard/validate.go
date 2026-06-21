package dashboard

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"time"

	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// paramError lets handlers map a parse failure to the request field it came
// from, which writeFieldError surfaces in the 400 response body.
type paramError struct {
	Field string
	Msg   string
}

func (e *paramError) Error() string { return e.Msg }

func badParam(field, format string, args ...any) *paramError {
	return &paramError{Field: field, Msg: fmt.Sprintf(format, args...)}
}

// parsePageParams pulls page/pageSize off the query string. Missing values
// fall back to the supplied defaults; junk values yield a paramError with
// the offending field name so the caller can return a precise 400.
func parsePageParams(q url.Values, defaultSize, maxSize int) (page, pageSize int, err *paramError) {
	page = 1
	pageSize = defaultSize
	if s := q.Get("page"); s != "" {
		n, perr := strconv.Atoi(s)
		if perr != nil || n < 1 {
			return 0, 0, badParam("page", "invalid page %q: must be a positive integer", s)
		}
		page = n
	}
	if s := q.Get("pageSize"); s != "" {
		n, perr := strconv.Atoi(s)
		if perr != nil || n < 1 || n > maxSize {
			return 0, 0, badParam("pageSize", "invalid pageSize %q: must be 1..%d", s, maxSize)
		}
		pageSize = n
	}
	return page, pageSize, nil
}

// parseEnumParam returns the param's value if it is in `allowed`, the empty
// string if the param was not supplied, and a paramError for any other
// value. Used for enum-shaped filters like ?risk=at-risk.
func parseEnumParam(q url.Values, name string, allowed []string) (string, *paramError) {
	v := q.Get(name)
	if v == "" {
		return "", nil
	}
	if slices.Contains(allowed, v) {
		return v, nil
	}
	return "", badParam(name, "invalid %s %q: must be one of %v", name, v, allowed)
}

// parseBoolParam parses ?name=true|false. Missing returns (nil, nil) so
// callers can distinguish "not supplied" from "supplied false".
func parseBoolParam(q url.Values, name string) (*bool, *paramError) {
	s := q.Get(name)
	if s == "" {
		return nil, nil
	}
	switch s {
	case "true":
		t := true
		return &t, nil
	case "false":
		f := false
		return &f, nil
	default:
		return nil, badParam(name, "invalid %s %q: must be 'true' or 'false'", name, s)
	}
}

// parseDurationParam validates a Prometheus-style ?window= duration
// (168h, 14d, etc.). Sub-minute units are rejected — they're nonsensical for
// the recommendation windows this dashboard surfaces.
func parseDurationParam(q url.Values, def string) (string, *paramError) {
	const name = "window"
	v := q.Get(name)
	if v == "" {
		return def, nil
	}
	if !windowPattern.MatchString(v) {
		return "", badParam(name, "invalid %s %q: must be a duration like 168h, 14d (no sub-minute units)", name, v)
	}
	return v, nil
}

// parseStepParam validates a chart resolution string. Sub-minute units are
// allowed here because chart resolution is a rendering concern, not a
// recommendation window.
func parseStepParam(q url.Values, def string) (string, *paramError) {
	v := q.Get("step")
	if v == "" {
		return def, nil
	}
	if !stepPattern.MatchString(v) {
		return "", badParam("step", "invalid step %q: must be a Prometheus duration (e.g. 30s, 5m, 1h)", v)
	}
	return v, nil
}

const maxSpanSeconds = int64(366 * 24 * 60 * 60)

// validateAbsoluteRange enforces from<to, a near-future upper bound on to,
// and a maximum span. Returns a *paramError (field "from"/"to") on failure.
func validateAbsoluteRange(from, to int64, now time.Time) *paramError {
	if from >= to {
		return badParam("from", "from (%d) must be before to (%d)", from, to)
	}
	if to > now.Add(time.Hour).Unix() {
		return badParam("to", "to is too far in the future")
	}
	if to-from > maxSpanSeconds {
		return badParam("from", "range too large (max 366d)")
	}
	return nil
}

// parseTimeRange resolves an absolute from/to (epoch seconds) range, or falls
// back to the relative ?window= (default defWindow). from/to win when present.
// Rules when from/to are supplied: both must be present, integer epoch seconds,
// from < to, to ≤ now+1h, and span ≤ 366 days.
func parseTimeRange(q url.Values, defWindow string, now time.Time) (promclient.TimeRange, *paramError) {
	fromStr, toStr := q.Get("from"), q.Get("to")
	if fromStr == "" && toStr == "" {
		window, perr := parseDurationParam(q, defWindow)
		if perr != nil {
			return promclient.TimeRange{}, perr
		}
		tr, err := promclient.TimeRangeFromWindow(window, now)
		if err != nil {
			return promclient.TimeRange{}, badParam("window", "%v", err)
		}
		return tr, nil
	}
	if fromStr == "" {
		return promclient.TimeRange{}, badParam("from", "from is required when to is supplied")
	}
	if toStr == "" {
		return promclient.TimeRange{}, badParam("to", "to is required when from is supplied")
	}
	from, err := strconv.ParseInt(fromStr, 10, 64)
	if err != nil {
		return promclient.TimeRange{}, badParam("from", "invalid from %q: must be epoch seconds", fromStr)
	}
	to, err := strconv.ParseInt(toStr, 10, 64)
	if err != nil {
		return promclient.TimeRange{}, badParam("to", "invalid to %q: must be epoch seconds", toStr)
	}
	if perr := validateAbsoluteRange(from, to, now); perr != nil {
		return promclient.TimeRange{}, perr
	}
	return promclient.TimeRange{Start: time.Unix(from, 0).UTC(), End: time.Unix(to, 0).UTC()}, nil
}
