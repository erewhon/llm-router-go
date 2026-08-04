package health

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
)

// Node schedules answer one question only: is this node *expected* to be off
// right now? Nothing here influences routing — routing follows observed
// availability. The point is that an archimedes powered down on a Saturday
// should read as "off (scheduled)" rather than as a red fault, on the
// dashboard and in /v1/availability.

var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday,
	"wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday,
	"sat": time.Saturday,
}

// window is one parsed "Mon-Fri 07:00-19:00" clause.
type window struct {
	days     map[time.Weekday]bool
	from, to int // minutes since midnight
}

// contains reports whether t falls inside the window, in t's own location.
// A window whose end is not after its start (e.g. "22:00-06:00") wraps past
// midnight, and the day set is matched against the day the window *started*.
func (w window) contains(t time.Time) bool {
	mins := t.Hour()*60 + t.Minute()
	if w.from < w.to {
		return w.days[t.Weekday()] && mins >= w.from && mins < w.to
	}
	// Wrapping window: either late on a listed day, or early on the day after.
	if w.days[t.Weekday()] && mins >= w.from {
		return true
	}
	prev := (t.Weekday() + 6) % 7
	return w.days[prev] && mins < w.to
}

// parseWindow parses "Mon-Fri 07:00-19:00", "Sat 10:00-14:00", or
// "Mon,Wed,Fri 08:30-17:00".
func parseWindow(spec string) (window, error) {
	fields := strings.Fields(strings.TrimSpace(spec))
	if len(fields) != 2 {
		return window{}, fmt.Errorf("want '<days> <HH:MM>-<HH:MM>', got %q", spec)
	}
	days, err := parseDays(fields[0])
	if err != nil {
		return window{}, err
	}
	from, to, err := parseTimeRange(fields[1])
	if err != nil {
		return window{}, err
	}
	return window{days: days, from: from, to: to}, nil
}

func parseDays(spec string) (map[time.Weekday]bool, error) {
	out := map[time.Weekday]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			start, err := parseDay(lo)
			if err != nil {
				return nil, err
			}
			end, err := parseDay(hi)
			if err != nil {
				return nil, err
			}
			// Walk forward so "Fri-Mon" wraps the weekend as written.
			for d := start; ; d = (d + 1) % 7 {
				out[d] = true
				if d == end {
					break
				}
			}
			continue
		}
		d, err := parseDay(part)
		if err != nil {
			return nil, err
		}
		out[d] = true
	}
	return out, nil
}

func parseDay(s string) (time.Weekday, error) {
	key := strings.ToLower(strings.TrimSpace(s))
	if len(key) > 3 {
		key = key[:3]
	}
	d, ok := weekdayNames[key]
	if !ok {
		return 0, fmt.Errorf("unknown day %q", s)
	}
	return d, nil
}

func parseTimeRange(spec string) (int, int, error) {
	lo, hi, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, fmt.Errorf("want '<HH:MM>-<HH:MM>', got %q", spec)
	}
	from, err := parseClock(lo)
	if err != nil {
		return 0, 0, err
	}
	to, err := parseClock(hi)
	if err != nil {
		return 0, 0, err
	}
	return from, to, nil
}

func parseClock(s string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, fmt.Errorf("want HH:MM, got %q", s)
	}
	hh, err := strconv.Atoi(h)
	if err != nil || hh < 0 || hh > 24 {
		return 0, fmt.Errorf("bad hour in %q", s)
	}
	mm, err := strconv.Atoi(m)
	if err != nil || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("bad minute in %q", s)
	}
	return hh*60 + mm, nil
}

// ExpectedDown reports whether a node is outside every declared uptime window
// at time t. A node with no schedule is always expected up. An unparseable
// window is ignored (and logged by the caller at load time) rather than
// silently marking a node as scheduled-off.
func ExpectedDown(n config.NodeDefinition, t time.Time) bool {
	if n.Schedule == nil || len(n.Schedule.ExpectedUp) == 0 {
		return false
	}
	var parsed int
	for _, spec := range n.Schedule.ExpectedUp {
		w, err := parseWindow(spec)
		if err != nil {
			continue
		}
		parsed++
		if w.contains(t) {
			return false
		}
	}
	// Every window failed to parse: assume up rather than mislabel the node.
	return parsed > 0
}

// ValidateSchedules returns an error for each unparseable window, so a typo in
// models.yaml surfaces at startup instead of silently disabling the badge.
func ValidateSchedules(reg *config.ModelRegistry) []error {
	var errs []error
	for name, n := range reg.Nodes {
		if n.Schedule == nil {
			continue
		}
		for _, spec := range n.Schedule.ExpectedUp {
			if _, err := parseWindow(spec); err != nil {
				errs = append(errs, fmt.Errorf("node %q: schedule.expected_up %q: %w", name, spec, err))
			}
		}
	}
	return errs
}

// nodeExpectedDownLocked is the tracker's internal helper. Caller holds t.mu.
func (t *Tracker) nodeExpectedDownLocked(node string) bool {
	if node == "" || t.cfg.Registry == nil {
		return false
	}
	n, ok := t.cfg.Registry.Nodes[node]
	if !ok {
		return false
	}
	return ExpectedDown(n, t.now())
}
