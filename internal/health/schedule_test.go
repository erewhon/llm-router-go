package health

import (
	"testing"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

func nodeWith(specs ...string) config.NodeDefinition {
	return config.NodeDefinition{Schedule: &config.NodeSchedule{ExpectedUp: specs}}
}

func TestExpectedDown_WeekdayWindow(t *testing.T) {
	n := nodeWith("Mon-Fri 07:00-19:00")
	cases := []struct {
		when string
		want bool
		why  string
	}{
		// 2026-08-03 is a Monday.
		{"2026-08-03 12:00", false, "Monday midday is inside the window"},
		{"2026-08-03 06:59", true, "Monday just before start"},
		{"2026-08-03 19:00", true, "end is exclusive"},
		{"2026-08-03 18:59", false, "Monday just before end"},
		{"2026-08-07 12:00", false, "Friday is in Mon-Fri"},
		{"2026-08-08 12:00", true, "Saturday is outside"},
		{"2026-08-09 12:00", true, "Sunday is outside"},
	}
	for _, tc := range cases {
		if got := ExpectedDown(n, at(t, tc.when)); got != tc.want {
			t.Errorf("%s: ExpectedDown(%s) = %v, want %v", tc.why, tc.when, got, tc.want)
		}
	}
}

func TestExpectedDown_MultipleWindows(t *testing.T) {
	n := nodeWith("Mon-Fri 07:00-19:00", "Sat 10:00-14:00")
	if ExpectedDown(n, at(t, "2026-08-08 11:00")) {
		t.Errorf("Saturday 11:00 should be inside the Sat window")
	}
	if !ExpectedDown(n, at(t, "2026-08-08 15:00")) {
		t.Errorf("Saturday 15:00 should be outside every window")
	}
}

func TestExpectedDown_WrappingWindow(t *testing.T) {
	// Overnight batch window: 22:00 Friday through 06:00 Saturday.
	n := nodeWith("Fri 22:00-06:00")
	if ExpectedDown(n, at(t, "2026-08-07 23:30")) {
		t.Errorf("Friday 23:30 should be inside a wrapping window")
	}
	if ExpectedDown(n, at(t, "2026-08-08 05:00")) {
		t.Errorf("Saturday 05:00 should be inside the window that started Friday")
	}
	if !ExpectedDown(n, at(t, "2026-08-08 07:00")) {
		t.Errorf("Saturday 07:00 is past the window end")
	}
}

func TestExpectedDown_CommaDaysAndWrapRange(t *testing.T) {
	if ExpectedDown(nodeWith("Mon,Wed,Fri 08:30-17:00"), at(t, "2026-08-05 09:00")) {
		t.Errorf("Wednesday 09:00 should be inside")
	}
	if !ExpectedDown(nodeWith("Mon,Wed,Fri 08:30-17:00"), at(t, "2026-08-04 09:00")) {
		t.Errorf("Tuesday is not listed")
	}
	// Fri-Mon wraps the weekend as written.
	weekend := nodeWith("Fri-Mon 00:00-23:59")
	for _, day := range []string{"2026-08-07 12:00", "2026-08-08 12:00", "2026-08-09 12:00", "2026-08-03 12:00"} {
		if ExpectedDown(weekend, at(t, day)) {
			t.Errorf("%s should be inside Fri-Mon", day)
		}
	}
	if !ExpectedDown(weekend, at(t, "2026-08-05 12:00")) {
		t.Errorf("Wednesday should be outside Fri-Mon")
	}
}

func TestExpectedDown_NoScheduleIsAlwaysUp(t *testing.T) {
	if ExpectedDown(config.NodeDefinition{}, at(t, "2026-08-08 03:00")) {
		t.Errorf("a node with no schedule is always expected up")
	}
	if ExpectedDown(nodeWith(), at(t, "2026-08-08 03:00")) {
		t.Errorf("an empty expected_up list is always expected up")
	}
}

func TestExpectedDown_UnparseableWindowAssumesUp(t *testing.T) {
	// A typo must not silently mark a node as scheduled-off — that would hide
	// a real outage behind an "off (scheduled)" badge.
	if ExpectedDown(nodeWith("Funday 07:00-19:00"), at(t, "2026-08-08 03:00")) {
		t.Errorf("an unparseable window should leave the node expected-up")
	}
}

func TestValidateSchedules(t *testing.T) {
	reg, err := config.LoadBytes([]byte(`
nodes:
  good:
    host: good.local
    gpu: nvidia
    vram_gb: 128
    schedule:
      expected_up: Mon-Fri 07:00-19:00
  bad:
    host: bad.local
    gpu: nvidia
    vram_gb: 128
    schedule:
      expected_up: [Mon-Fri 7am-7pm]
models:
  m: {hf_repo: a/b, node: good}
`))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	// The scalar form must decode as a one-element list.
	if got := reg.Nodes["good"].Schedule.ExpectedUp; len(got) != 1 || got[0] != "Mon-Fri 07:00-19:00" {
		t.Errorf("scalar expected_up = %v, want one-element list", got)
	}
	errs := ValidateSchedules(reg)
	if len(errs) != 1 {
		t.Fatalf("got %d validation errors, want 1: %v", len(errs), errs)
	}
}

func TestTrackerMarksExpectedDown(t *testing.T) {
	reg, err := config.LoadBytes([]byte(`
nodes:
  archimedes:
    host: archimedes.local
    gpu: nvidia
    vram_gb: 128
    schedule:
      expected_up: Mon-Fri 07:00-19:00
models:
  thinker-model: {hf_repo: a/b, node: archimedes}
`))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	saturday := at(t, "2026-08-08 03:00")
	fleet := &fakeFleet{down: map[string]bool{"archimedes": true}}
	tr := NewTracker(Config{
		Registry:        reg,
		Probe:           fleet.probe(map[string]string{"archimedes.local": "archimedes"}),
		GenerationProbe: probeOK,
		Now:             func() time.Time { return saturday },
	})
	tr.PollOnce(t.Context())
	tr.PollOnce(t.Context())

	snap := tr.Snapshot()
	if len(snap) != 1 || !snap[0].ExpectedDown {
		t.Errorf("model on a scheduled-off node should carry expected_down: %+v", snap)
	}
	// Scheduling is cosmetic: the model is still genuinely unavailable.
	if snap[0].State != Unavailable {
		t.Errorf("state = %q, want unavailable — schedule must not affect routing", snap[0].State)
	}
}
