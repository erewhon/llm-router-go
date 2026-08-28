package router

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// ---------------------------------------------------------------------------
// classification
// ---------------------------------------------------------------------------

type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "i/o timeout" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return true }

func TestClassifyTransportErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"net timeout", fakeTimeoutErr{}, "timeout"},
		{"dial refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "connect"},
		{"bare refused", syscall.ECONNREFUSED, "connect"},
		{"host unreachable", syscall.EHOSTUNREACH, "connect"},
		{"reset mid body", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, "transport"},
		{"generic", errors.New("http: server closed idle connection"), "transport"},
	}
	for _, c := range cases {
		if got := classifyTransportErr(c.err); got != c.want {
			t.Errorf("%s: classifyTransportErr = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestHasErrorEnvelope(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"object error", `{"error":{"type":"api_error","message":"upstream broke"}}`, true},
		{"string error", `{"error":"nope"}`, true},
		{"null error", `{"error":null}`, false},
		{"no error", `{"id":"x","choices":[]}`, false},
		{"invalid json", `data: [DONE]`, false},
	}
	for _, c := range cases {
		if got := hasErrorEnvelope([]byte(c.body)); got != c.want {
			t.Errorf("%s: hasErrorEnvelope = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestErrorClassOf(t *testing.T) {
	cases := []struct {
		name      string
		transport string
		status    int
		envelope  bool
		want      string
	}{
		{"success", "", 200, false, ""},
		{"never attempted", "", 0, false, ""},
		{"transport wins over status", "timeout", 200, false, "timeout"},
		{"5xx", "", 500, false, "server_error"},
		{"4xx", "", 429, false, "client_error"},
		{"envelope in 200", "", 200, true, "error_envelope"},
	}
	for _, c := range cases {
		if got := errorClassOf(c.transport, c.status, c.envelope); got != c.want {
			t.Errorf("%s: errorClassOf = %q, want %q", c.name, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// sliding-window tracker
// ---------------------------------------------------------------------------

func TestUpstreamTracker_WindowsAndStreak(t *testing.T) {
	tr := newUpstreamTracker()
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	// 25h ago: outside the 24h window entirely.
	tr.record("m", "https://zen/v1", "server_error", base.Add(-25*time.Hour))
	// 2h ago: in 24h, out of 1h.
	tr.record("m", "https://zen/v1", "", base.Add(-2*time.Hour))
	tr.record("m", "https://zen/v1", "server_error", base.Add(-2*time.Hour))
	// 10m ago: in both windows.
	tr.record("m", "https://zen/v1", "timeout", base.Add(-10*time.Minute))

	rows := tr.stats(base)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Total24h != 3 || r.Failed24h != 2 {
		t.Errorf("24h = %d/%d, want 2/3 failed/total", r.Failed24h, r.Total24h)
	}
	if r.Total1h != 1 || r.Failed1h != 1 {
		t.Errorf("1h = %d/%d, want 1/1 failed/total", r.Failed1h, r.Total1h)
	}
	if r.LastErrorClass != "timeout" {
		t.Errorf("LastErrorClass = %q, want timeout", r.LastErrorClass)
	}
	// The success 2h ago reset the streak; the streak restarts at the
	// server_error immediately after it.
	if r.FailingSince == nil || !r.FailingSince.Equal(base.Add(-2*time.Hour)) {
		t.Errorf("FailingSince = %v, want %v", r.FailingSince, base.Add(-2*time.Hour))
	}

	// A success now ends the streak but keeps the failure counts.
	tr.record("m", "https://zen/v1", "", base)
	rows = tr.stats(base)
	if rows[0].FailingSince != nil {
		t.Errorf("FailingSince = %v after success, want nil", rows[0].FailingSince)
	}
	if rows[0].Failed24h != 2 {
		t.Errorf("Failed24h = %d after success, want 2", rows[0].Failed24h)
	}
}

func TestUpstreamTracker_ClientErrorNeitherFailsNorResets(t *testing.T) {
	tr := newUpstreamTracker()
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	tr.record("m", "b", "server_error", base.Add(-5*time.Minute))
	tr.record("m", "b", "client_error", base.Add(-4*time.Minute))
	rows := tr.stats(base)
	if rows[0].Failed1h != 1 {
		t.Errorf("Failed1h = %d, want 1 (client_error must not count)", rows[0].Failed1h)
	}
	if rows[0].FailingSince == nil {
		t.Errorf("FailingSince cleared by client_error; must persist")
	}
}

func TestUpstreamTracker_SortsFailuresFirst(t *testing.T) {
	tr := newUpstreamTracker()
	now := time.Now()
	for i := 0; i < 10; i++ {
		tr.record("healthy-busy", "b", "", now)
	}
	tr.record("broken", "b", "connect", now)
	rows := tr.stats(now)
	if len(rows) != 2 || rows[0].Model != "broken" {
		t.Fatalf("rows[0] = %+v, want the failing model first", rows)
	}
}

// ---------------------------------------------------------------------------
// end-to-end through handleProxy
// ---------------------------------------------------------------------------

func TestReqlog_Upstream500Classified(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"type":"api_error","message":"boom"}}`))
	}))
	defer up.Close()
	sink := &reqlog.MemorySink{}
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport}, WithSink(sink))

	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"zen-glm","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 proxied", rec.Code)
	}
	r := sink.Records()[0]
	if r.UpstreamStatus != 500 {
		t.Errorf("UpstreamStatus = %d, want 500", r.UpstreamStatus)
	}
	if r.ErrorClass != "server_error" {
		t.Errorf("ErrorClass = %q, want server_error", r.ErrorClass)
	}
	rows := rt.upstreamStats.stats(time.Now())
	if len(rows) != 1 || rows[0].Model != "zen-glm" || rows[0].Failed1h != 1 {
		t.Errorf("upstreamStats rows = %+v, want one zen-glm failure", rows)
	}
}

func TestReqlog_ErrorEnvelopeIn200Classified(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"quota exceeded"}}`))
	}))
	defer up.Close()
	sink := &reqlog.MemorySink{}
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport}, WithSink(sink))

	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"zen-glm","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 passthrough", rec.Code)
	}
	r := sink.Records()[0]
	if r.UpstreamStatus != 200 {
		t.Errorf("UpstreamStatus = %d, want 200", r.UpstreamStatus)
	}
	if r.ErrorClass != "error_envelope" {
		t.Errorf("ErrorClass = %q, want error_envelope", r.ErrorClass)
	}
}

// errTransport fails every request at the transport with the given error.
type errTransport struct{ err error }

func (e errTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.err }

func TestReqlog_TransportFailureClassified(t *testing.T) {
	sink := &reqlog.MemorySink{}
	dialErr := &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	rt := newTestRouter(t, errTransport{err: dialErr}, WithSink(sink))

	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"zen-glm","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	r := sink.Records()[0]
	if r.UpstreamStatus != 0 {
		t.Errorf("UpstreamStatus = %d, want 0 (never answered)", r.UpstreamStatus)
	}
	if r.ErrorClass != "connect" {
		t.Errorf("ErrorClass = %q, want connect", r.ErrorClass)
	}
	rows := rt.upstreamStats.stats(time.Now())
	if len(rows) != 1 || rows[0].Failed1h != 1 || rows[0].FailingSince == nil {
		t.Errorf("upstreamStats rows = %+v, want one failing zen-glm row with FailingSince", rows)
	}
}

func TestReqlog_RejectedRequestHasNoUpstreamOutcome(t *testing.T) {
	sink := &reqlog.MemorySink{}
	rt := newTestRouter(t, nil, WithSink(sink))

	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"no-such-model","messages":[]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	r := sink.Records()[0]
	if r.UpstreamStatus != 0 || r.ErrorClass != "" {
		t.Errorf("rejected request leaked upstream outcome: status=%d class=%q",
			r.UpstreamStatus, r.ErrorClass)
	}
	if rows := rt.upstreamStats.stats(time.Now()); len(rows) != 0 {
		t.Errorf("upstreamStats rows = %+v, want none for a pre-upstream rejection", rows)
	}
}
