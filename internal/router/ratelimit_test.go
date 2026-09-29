package router

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// gatewayYAML is the laptop shape: two models behind one corporate gateway,
// the same small model served on this machine, and a role that prefers the
// gateway. `planner` has a single candidate on purpose — nothing to fall
// back to, so whatever the gateway says must reach the caller.
const gatewayYAML = `
models:
  gw/glm:
    hf_repo: glm-flash
    backend: external
    api_base: https://gw-glm.example/v1
    api_key: sk-literal-gw
    capabilities: [text, tool_calling]

  gw/qwen:
    hf_repo: qwen-27b
    backend: external
    api_base: https://gw-qwen.example/v1
    api_key: sk-literal-gw
    capabilities: [text, tool_calling]

  local/qwen:
    hf_repo: qwen-27b-mlx
    backend: external
    api_base: http://laptop.example/v1
    capabilities: [text, tool_calling]

  gw/opus:
    hf_repo: opus
    backend: external
    api_base: https://gw-opus.example/v1
    api_key: sk-literal-gw
    capabilities: [text, tool_calling]

roles:
  coder:
    require: {capabilities: [text, tool_calling]}
    candidates: [gw/glm, gw/qwen, local/qwen]

  planner:
    require: {capabilities: [text, tool_calling]}
    candidates: [gw/opus]
`

// errAvailability records what the router reports, errors included, so a
// test can see the Retry-After the tracker would have been handed.
type errAvailability struct {
	failures  map[string]error
	successes []string
}

func (a *errAvailability) Routable(string) bool { return true }
func (a *errAvailability) Reason(string) string { return "available (poll)" }
func (a *errAvailability) ReportFailure(id string, err error) {
	if a.failures == nil {
		a.failures = map[string]error{}
	}
	a.failures[id] = err
}
func (a *errAvailability) ReportSuccess(id string) { a.successes = append(a.successes, id) }

func newGatewayRouter(t *testing.T, byHost map[string]string, extra ...Option) (*Router, *errAvailability) {
	t.Helper()
	reg, err := config.LoadBytes([]byte(gatewayYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	avail := &errAvailability{}
	opts := append([]Option{
		WithAvailability(avail),
		WithFlushInterval(0),
		WithTransport(&hostTransport{byHost: byHost}),
	}, extra...)
	return New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)), opts...), avail
}

// refusing answers every request with status and the given headers, and
// counts how often it was asked.
func refusing(t *testing.T, status int, headers map[string]string, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

const coderRequest = `{"model":"coder","messages":[{"role":"user","content":"hi"}]}`

func TestRoleFailsOverOn429ToTheLocalSeat(t *testing.T) {
	// The motivating case: the gateway is out of budget for everything it
	// serves, and the model on this machine should take the work.
	glm, glmHits := refusing(t, 429, map[string]string{"Retry-After": "30"}, `{"error":{"message":"budget exceeded"}}`)
	qwen, qwenHits := refusing(t, 429, nil, `{"error":{"message":"budget exceeded"}}`)
	local, localHits := refusing(t, 200, nil, okBody)

	sink := &reqlog.MemorySink{}
	rt, avail := newGatewayRouter(t, map[string]string{
		"gw-glm.example":  glm.URL,
		"gw-qwen.example": qwen.URL,
		"laptop.example":  local.URL,
	}, WithSink(sink))

	rec := postTo(t, rt, "/v1/chat/completions", coderRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the local seat; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Router-Resolved"); got != "local/qwen" {
		t.Errorf("X-Router-Resolved = %q, want local/qwen", got)
	}
	if glmHits.Load() != 1 || qwenHits.Load() != 1 || localHits.Load() != 1 {
		t.Errorf("hits glm=%d qwen=%d local=%d, want one each, in order",
			glmHits.Load(), qwenHits.Load(), localHits.Load())
	}

	// The tracker is told about both refusals, with the cooldown the gateway
	// named where it named one.
	var es *errUpstreamStatus
	if !errors.As(avail.failures["gw/glm"], &es) || es.Status != 429 || es.RetryAfter() != 30*time.Second {
		t.Errorf("gw/glm failure = %v, want a 429 carrying retry-after 30s", avail.failures["gw/glm"])
	}
	if !errors.As(avail.failures["gw/qwen"], &es) || es.Status != 429 || es.RetryAfter() != 0 {
		t.Errorf("gw/qwen failure = %v, want a 429 with no retry-after", avail.failures["gw/qwen"])
	}
	if len(avail.successes) != 1 || avail.successes[0] != "local/qwen" {
		t.Errorf("successes = %v, want [local/qwen]", avail.successes)
	}

	r := sink.Records()[0]
	if r.ResolvedVia != "local/qwen" || r.Status != 200 || r.ErrorClass != "" {
		t.Errorf("record resolved=%q status=%d class=%q, want a clean 200 from local/qwen",
			r.ResolvedVia, r.Status, r.ErrorClass)
	}
	rows := map[string]upstreamRow{}
	for _, row := range rt.upstreamStats.stats(time.Now()) {
		rows[row.Model] = row
	}
	if rows["gw/glm"].Failed1h != 1 || rows["gw/glm"].LastErrorClass != errorClassRateLimited {
		t.Errorf("gw/glm stats = %+v, want one rate_limited failure", rows["gw/glm"])
	}
}

func TestRoleFailsOverOn500(t *testing.T) {
	// A role used to retry only when the upstream never answered; a gateway
	// that answers 500 is just as unable to serve the request.
	glm, _ := refusing(t, 500, nil, `{"error":{"message":"gateway exploded"}}`)
	qwen, _ := refusing(t, 200, nil, okBody)
	rt, avail := newGatewayRouter(t, map[string]string{
		"gw-glm.example":  glm.URL,
		"gw-qwen.example": qwen.URL,
	})

	rec := postTo(t, rt, "/v1/chat/completions", coderRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Router-Resolved"); got != "gw/qwen" {
		t.Errorf("X-Router-Resolved = %q, want gw/qwen", got)
	}
	if _, reported := avail.failures["gw/glm"]; !reported {
		t.Errorf("the 500 was not reported against gw/glm")
	}
}

func TestLastCandidate429PassesThrough(t *testing.T) {
	// Every seat refuses. The caller must see the last seat's own 429 and its
	// Retry-After, not a synthetic 502 that hides why.
	refuse := map[string]string{"Retry-After": "7"}
	glm, _ := refusing(t, 429, nil, `{"error":{"message":"glm refused"}}`)
	qwen, _ := refusing(t, 429, nil, `{"error":{"message":"qwen refused"}}`)
	local, _ := refusing(t, 429, refuse, `{"error":{"message":"local refused"}}`)
	rt, _ := newGatewayRouter(t, map[string]string{
		"gw-glm.example":  glm.URL,
		"gw-qwen.example": qwen.URL,
		"laptop.example":  local.URL,
	})

	rec := postTo(t, rt, "/v1/chat/completions", coderRequest)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the final seat's 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "local refused") {
		t.Errorf("body = %q, want the final seat's error body", rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q, want the final seat's 7", got)
	}
}

func TestSingleCandidateRole429PassesThrough(t *testing.T) {
	opus, hits := refusing(t, 429, map[string]string{"Retry-After": "12"}, `{"error":{"message":"opus refused"}}`)
	sink := &reqlog.MemorySink{}
	rt, avail := newGatewayRouter(t, map[string]string{"gw-opus.example": opus.URL}, WithSink(sink))

	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"planner","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "opus refused") {
		t.Fatalf("status = %d body = %q, want the gateway's own 429", rec.Code, rec.Body.String())
	}
	if hits.Load() != 1 {
		t.Errorf("gateway asked %d times, want 1 (nothing to fail over to, no retry)", hits.Load())
	}
	if len(avail.failures) != 0 {
		t.Errorf("failures = %v, want none reported for a passed-through response", avail.failures)
	}
	if r := sink.Records()[0]; r.ErrorClass != errorClassRateLimited {
		t.Errorf("error class = %q, want rate_limited", r.ErrorClass)
	}
}

func TestNamedModel429PassesThrough(t *testing.T) {
	// Naming a model is a statement about that model: no walk, no retry.
	glm, _ := refusing(t, 429, nil, `{"error":{"message":"glm refused"}}`)
	local, localHits := refusing(t, 200, nil, okBody)
	rt, _ := newGatewayRouter(t, map[string]string{
		"gw-glm.example": glm.URL,
		"laptop.example": local.URL,
	})

	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"gw/glm","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 passthrough", rec.Code)
	}
	if localHits.Load() != 0 {
		t.Errorf("the local seat served a request that named gw/glm")
	}
}

func TestRole4xxOtherThan429IsNotRetried(t *testing.T) {
	// A 400 is the request's own fault and would fail the same way anywhere.
	glm, _ := refusing(t, 400, nil, `{"error":{"message":"context length exceeded"}}`)
	qwen, qwenHits := refusing(t, 200, nil, okBody)
	rt, avail := newGatewayRouter(t, map[string]string{
		"gw-glm.example":  glm.URL,
		"gw-qwen.example": qwen.URL,
	})

	rec := postTo(t, rt, "/v1/chat/completions", coderRequest)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the 400 passed through", rec.Code)
	}
	if qwenHits.Load() != 0 || len(avail.failures) != 0 {
		t.Errorf("a 400 moved the request on: qwen hits=%d failures=%v", qwenHits.Load(), avail.failures)
	}
}

func TestChainFailsOverOn429(t *testing.T) {
	primary, _ := refusing(t, 429, map[string]string{"Retry-After": "5"}, `{"error":{"message":"slow down"}}`)
	secondary, _ := refusing(t, 200, nil, okBody)
	rt := newChainRouter(t, &hostTransport{byHost: map[string]string{
		"or.example":  primary.URL,
		"zen.example": secondary.URL,
	}})

	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"k3","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the fallback provider", rec.Code)
	}
	if got := rec.Header().Get("X-Router-Resolved"); got != "zen/k3" {
		t.Errorf("X-Router-Resolved = %q, want zen/k3", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"absent", "", 0},
		{"seconds", "30", 30 * time.Second},
		{"padded", " 30 ", 30 * time.Second},
		{"fractional", "1.5", 1500 * time.Millisecond},
		{"zero", "0", 0},
		{"negative", "-5", 0},
		{"garbage", "soon", 0},
		{"absurd", "1e300", 0},
		{"http date ahead", now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{"http date behind", now.Add(-time.Minute).Format(http.TimeFormat), 0},
	}
	for _, c := range cases {
		if got := parseRetryAfter(c.in, now); got != c.want {
			t.Errorf("%s: parseRetryAfter(%q) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

func TestClassifyUpstreamErrSeparates429(t *testing.T) {
	cases := map[int]string{500: "server_error", 503: "server_error", 429: errorClassRateLimited}
	for status, want := range cases {
		if got := classifyUpstreamErr(&errUpstreamStatus{Status: status}); got != want {
			t.Errorf("status %d classified %q, want %q", status, got, want)
		}
	}
	if !isUpstreamFailure(errorClassRateLimited) {
		t.Errorf("rate_limited must count toward a seat's failure rate")
	}
}
