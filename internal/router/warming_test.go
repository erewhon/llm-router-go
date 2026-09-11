package router

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/health"
)

// status503 answers every request with the llama-server "Loading model" 503.
type status503 struct{}

func (status503) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"Loading model","code":503}}`)),
		Request:    req,
	}, nil
}

// A 503 that passes through from a fleet seat is reported to the tracker as a
// failure (it will demote the seat to warming), not as a success — the bug
// that had the router confirming a loading engine on every request it 503'd.
func TestPassthrough503FromLocalSeatIsNotASuccess(t *testing.T) {
	rt, avail := newRoleRouter(t, nil, WithTransport(status503{}))
	srv := httptest.NewServer(rt.Handler())
	defer srv.Close()

	// A directly named local model: no failover, the 503 streams through.
	resp := postChatHTTP(t, srv.URL, `{"model":"minimax-reap","messages":[]}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the upstream 503 passed through", resp.StatusCode)
	}
	if len(avail.successes) != 0 {
		t.Errorf("a 503 must not be reported as a success; got successes=%v", avail.successes)
	}
	if len(avail.failures) != 1 || avail.failures[0] != "minimax-reap" {
		t.Errorf("failures = %v, want [minimax-reap]", avail.failures)
	}
}

// The same 503 from a nodeless external is the provider's business: the
// tracker sees it exactly as before (a proxied response, i.e. a success).
func TestPassthrough503FromExternalStaysASuccess(t *testing.T) {
	rt, avail := newRoleRouter(t, nil, WithTransport(status503{}))
	srv := httptest.NewServer(rt.Handler())
	defer srv.Close()

	resp := postChatHTTP(t, srv.URL, `{"model":"kimi-cloud","messages":[]}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 passed through", resp.StatusCode)
	}
	if len(avail.failures) != 0 {
		t.Errorf("an external's 503 must not be reported as a failure; got %v", avail.failures)
	}
	if len(avail.successes) != 1 || avail.successes[0] != "kimi-cloud" {
		t.Errorf("successes = %v, want [kimi-cloud]", avail.successes)
	}
}

// A role whose only compliant candidate is warming answers 503 with a reason
// that says so — the operator-facing half of the probe.
func TestRoleReasonNamesAWarmingSeat(t *testing.T) {
	rt, avail := newRoleRouter(t, map[string]bool{"minimax-reap": true, "qwen36-hypatia": true})
	avail.reasons = map[string]string{
		"minimax-reap":   "warming (probe: generation probe: upstream status 503: Loading model)",
		"qwen36-hypatia": "unavailable (poll: node unreachable)",
	}
	srv := httptest.NewServer(rt.Handler())
	defer srv.Close()

	resp := postChatHTTP(t, srv.URL, `{"model":"thinker","messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "minimax-reap: warming (probe:") {
		t.Errorf("503 body should name the warming seat and why; got %s", body)
	}
}

// /health carries the verdict tallies when a reporter is attached.
func TestHealthReportsAvailabilityCounts(t *testing.T) {
	rt, _ := newRoleRouter(t, nil, WithAvailability(&countingReporter{
		stubAvailability: stubAvailability{down: map[string]bool{}},
		states:           map[string]string{"minimax-reap": "warming", "qwen36-hypatia": "available"},
	}))
	srv := httptest.NewServer(rt.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc struct {
		Availability map[string]int `json:"availability"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Availability["warming"] != 1 || doc.Availability["available"] < 1 {
		t.Errorf("availability tallies = %v, want warming=1 and available>=1", doc.Availability)
	}
}

// countingReporter is a stubAvailability that also implements the reporter
// half, with a fixed per-model state map.
type countingReporter struct {
	stubAvailability
	states map[string]string
}

func (c *countingReporter) Snapshot() []health.Status {
	out := make([]health.Status, 0, len(c.states))
	for id, st := range c.states {
		out = append(out, health.Status{Model: id, State: health.Availability(st)})
	}
	return out
}

func (c *countingReporter) NodeStatuses() []health.NodeStatus { return nil }

func postChatHTTP(t *testing.T, base, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}
