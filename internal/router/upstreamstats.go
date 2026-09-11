package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"syscall"
	"time"
)

// Upstream failure tracking — the instrumentation motivated by the 2026-08-27
// Zen incident, where every claude-* id served 500s for 20+ hours while
// glm-5.3 on the same endpoint stayed healthy and nothing surfaced it. The
// pieces here answer "which models are failing upstream, at what rate, since
// when, and is it one model or the whole endpoint": a transport-error
// classifier, an error-envelope-in-2xx detector, and an in-memory sliding
// window feeding the dashboard's /api/upstream. Durable history lives in
// reqlog (upstream_status/error_class columns); this window exists so the
// dashboard needs no Postgres round-trip.

// classifyTransportErr maps a reverseProxyTo transport error to an outcome
// class: "timeout" (deadline/timeout), "connect" (dial-phase failure — the
// backend host is down or refusing), or "transport" (everything else: reset
// mid-body, TLS failure, malformed response). Returns "" for nil.
func classifyTransportErr(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return "connect"
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return "connect"
	}
	return "transport"
}

// errUpstreamStatus marks a retryable upstream 5xx whose response the chain
// failover path suppressed so the next provider could take the request. It
// flows out of ModifyResponse through ReverseProxy's ErrorHandler into the
// handleProxy retry loop.
type errUpstreamStatus struct{ Status int }

func (e *errUpstreamStatus) Error() string {
	return fmt.Sprintf("upstream status %d", e.Status)
}

// UpstreamStatus exposes the status to the health tracker, which reads it
// through a small interface rather than this package's type: a 503 from a
// probed seat means "warming", not "trip the breaker".
func (e *errUpstreamStatus) UpstreamStatus() int { return e.Status }

// errUpstreamEnvelope marks a suppressed error-envelope-in-2xx response —
// the failure shape that looks like success until the body is read.
var errUpstreamEnvelope = errors.New("upstream error envelope in 2xx")

// classifyUpstreamErr extends classifyTransportErr with the chain-failover
// sentinels, so a suppressed 5xx / envelope classifies the same way a
// passed-through one would have.
func classifyUpstreamErr(err error) string {
	var es *errUpstreamStatus
	if errors.As(err, &es) {
		if es.Status >= 500 {
			return "server_error"
		}
		return "client_error"
	}
	if errors.Is(err, errUpstreamEnvelope) {
		return "error_envelope"
	}
	return classifyTransportErr(err)
}

// hasErrorEnvelope reports whether a JSON body carries a top-level non-null
// "error" member. Providers occasionally serve these inside an HTTP 2xx; a
// caller retrying on status alone never notices.
func hasErrorEnvelope(body []byte) bool {
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return false
	}
	return len(probe.Error) > 0 && string(probe.Error) != "null"
}

// errorClassOf derives the reqlog ErrorClass for a request's final upstream
// attempt. A transport failure wins over whatever status was seen (a stream
// that dies mid-body failed, even though the status line was 200). Empty
// means success — or that no upstream was ever attempted (status 0).
func errorClassOf(transportClass string, upstreamStatus int, envelope bool) string {
	switch {
	case transportClass != "":
		return transportClass
	case upstreamStatus >= 500:
		return "server_error"
	case upstreamStatus >= 400:
		return "client_error"
	case envelope:
		return "error_envelope"
	default:
		return ""
	}
}

// errorClassPrivacyRefused marks a request the privacy tier turned away
// before any upstream was tried. It lives in the same column as the upstream
// classes so one query covers every non-success outcome, but it is not one of
// them: nothing was sent.
const errorClassPrivacyRefused = "privacy_refused"

// isUpstreamFailure reports whether an error class counts toward a model's
// upstream failure rate. client_error (4xx) is excluded: it is almost always
// the caller's request at fault (bad params, context overflow), and counting
// it would make a healthy endpoint look degraded under a misbehaving client.
// privacy_refused is excluded because no upstream was involved at all.
func isUpstreamFailure(class string) bool {
	return class != "" && class != "client_error" && !isPolicyRefusal(class)
}

// isPolicyRefusal reports whether an error class means the router itself
// turned the request away on policy — a privacy tier or a token scope —
// before any upstream was involved. Neither counts against a seat.
func isPolicyRefusal(class string) bool {
	return class == errorClassPrivacyRefused || class == errorClassScopeRefused
}

// ---------------------------------------------------------------------------
// Sliding-window tracker
// ---------------------------------------------------------------------------

const upstreamWindowMinutes = 24 * 60

// upstreamTracker keeps per-(model, endpoint) minute buckets over a rolling
// 24h window, plus streak state answering "failing since when". In-memory
// only: a router restart clears it, and the canned reqlog query is the
// durable fallback. Memory is bounded: ~70 model/endpoint pairs x 1440
// twelve-byte buckets.
type upstreamTracker struct {
	mu sync.Mutex
	m  map[upstreamKey]*upstreamSeries
}

type upstreamKey struct {
	model   string
	apiBase string
}

type upstreamBucket struct {
	minute        int64 // unix minute this bucket currently holds
	total, failed int32
}

type upstreamSeries struct {
	buckets [upstreamWindowMinutes]upstreamBucket
	// failingSince is the start of the current unbroken failure streak; zero
	// when the most recent attempt succeeded.
	failingSince   time.Time
	lastFailureAt  time.Time
	lastErrorClass string
}

func newUpstreamTracker() *upstreamTracker {
	return &upstreamTracker{m: make(map[upstreamKey]*upstreamSeries)}
}

// record notes one upstream attempt. class is the attempt's error class (""
// for success); failed failover attempts are recorded individually by the
// proxy loop, so a request that walked three candidates contributes three
// attempts to three series.
func (t *upstreamTracker) record(model, apiBase, class string, now time.Time) {
	if t == nil || model == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := upstreamKey{model: model, apiBase: apiBase}
	s := t.m[key]
	if s == nil {
		s = &upstreamSeries{}
		t.m[key] = s
	}
	minute := now.Unix() / 60
	b := &s.buckets[minute%upstreamWindowMinutes]
	if b.minute != minute {
		*b = upstreamBucket{minute: minute}
	}
	b.total++
	if isUpstreamFailure(class) {
		b.failed++
		s.lastFailureAt = now
		s.lastErrorClass = class
		if s.failingSince.IsZero() {
			s.failingSince = now
		}
	} else if class == "" {
		// A success ends the streak. client_error neither ends nor starts one:
		// it says nothing about upstream health either way.
		s.failingSince = time.Time{}
	}
}

// upstreamRow is one (model, endpoint) line of the dashboard payload.
type upstreamRow struct {
	Model          string     `json:"model"`
	APIBase        string     `json:"api_base"`
	Total1h        int        `json:"total_1h"`
	Failed1h       int        `json:"failed_1h"`
	Total24h       int        `json:"total_24h"`
	Failed24h      int        `json:"failed_24h"`
	LastErrorClass string     `json:"last_error_class,omitempty"`
	FailingSince   *time.Time `json:"failing_since,omitempty"`
	LastFailureAt  *time.Time `json:"last_failure_at,omitempty"`
}

// stats returns every series with traffic in the window, models with recent
// failures first (then by 24h volume) so the dashboard's problem rows are on
// top without client-side sorting.
func (t *upstreamTracker) stats(now time.Time) []upstreamRow {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	minute := now.Unix() / 60
	rows := make([]upstreamRow, 0, len(t.m))
	for key, s := range t.m {
		row := upstreamRow{Model: key.model, APIBase: key.apiBase}
		for i := range s.buckets {
			b := &s.buckets[i]
			age := minute - b.minute
			if age < 0 || age >= upstreamWindowMinutes || b.total == 0 {
				continue
			}
			row.Total24h += int(b.total)
			row.Failed24h += int(b.failed)
			if age < 60 {
				row.Total1h += int(b.total)
				row.Failed1h += int(b.failed)
			}
		}
		if row.Total24h == 0 {
			continue
		}
		if !s.failingSince.IsZero() {
			fs := s.failingSince
			row.FailingSince = &fs
		}
		if !s.lastFailureAt.IsZero() && now.Sub(s.lastFailureAt) < upstreamWindowMinutes*time.Minute {
			lf := s.lastFailureAt
			row.LastFailureAt = &lf
			row.LastErrorClass = s.lastErrorClass
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if (rows[i].Failed24h > 0) != (rows[j].Failed24h > 0) {
			return rows[i].Failed24h > 0
		}
		if rows[i].Total24h != rows[j].Total24h {
			return rows[i].Total24h > rows[j].Total24h
		}
		if rows[i].Model != rows[j].Model {
			return rows[i].Model < rows[j].Model
		}
		return rows[i].APIBase < rows[j].APIBase
	})
	return rows
}
