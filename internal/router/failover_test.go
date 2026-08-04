package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// deadBackends is a RoundTripper that refuses connections to the hosts named
// in `dead` and serves a canned 200 from everything else — simulating a node
// that has been powered off rather than one that answers with an error.
type deadBackends struct {
	dead map[string]bool

	mu   sync.Mutex
	seen []string // hosts dialed, in order
}

func (d *deadBackends) RoundTrip(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.seen = append(d.seen, req.URL.Host)
	d.mu.Unlock()

	if d.dead[req.URL.Host] {
		return nil, fmt.Errorf("dial tcp %s: connect: connection refused", req.URL.Host)
	}
	body := fmt.Sprintf(`{"id":"x","choices":[],"served_by":%q,"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`, req.URL.Host)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func (d *deadBackends) dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

func TestFailoverAdvancesOnDialFailure(t *testing.T) {
	// The tracker still believes hypatia is up (it has not polled since the
	// machine went away). The request must not simply 502 — it should notice,
	// report the failure, and move to the next candidate.
	tp := &deadBackends{dead: map[string]bool{"hypatia.local:5391": true}}
	rt, avail := newRoleRouter(t, nil, WithTransport(tp))

	rec := postTo(t, rt, "/v1/chat/completions", `{"model":"coder","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover. body=%s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["served_by"] != "archimedes.local:5391" {
		t.Errorf("served_by = %v, want archimedes.local:5391", got["served_by"])
	}

	dialed := tp.dialed()
	if len(dialed) != 2 || dialed[0] != "hypatia.local:5391" {
		t.Errorf("dialed = %v, want hypatia then archimedes", dialed)
	}

	// The failure must be fed back so the next request skips hypatia outright.
	if len(avail.failures) != 1 || avail.failures[0] != "qwen36-hypatia" {
		t.Errorf("reported failures = %v, want [qwen36-hypatia]", avail.failures)
	}
	if len(avail.successes) != 1 || avail.successes[0] != "minimax-reap" {
		t.Errorf("reported successes = %v, want [minimax-reap]", avail.successes)
	}
}

func TestFailoverHeadersNameTheFinalModel(t *testing.T) {
	tp := &deadBackends{dead: map[string]bool{"hypatia.local:5391": true}}
	rt, _ := newRoleRouter(t, nil, WithTransport(tp))

	rec := postTo(t, rt, "/v1/chat/completions", `{"model":"coder","messages":[]}`)
	if got := rec.Header().Get("X-Router-Role"); got != "coder" {
		t.Errorf("X-Router-Role = %q, want coder", got)
	}
	// Must be the model that actually answered, not the one we first chose.
	if got := rec.Header().Get("X-Router-Resolved"); got != "minimax-reap" {
		t.Errorf("X-Router-Resolved = %q, want minimax-reap", got)
	}
	if got := rec.Header().Get("X-Router-Overflow"); got != "" {
		t.Errorf("X-Router-Overflow = %q, want empty (no boundary crossed)", got)
	}
}

func TestFailoverStopsAtTheAttemptCap(t *testing.T) {
	// Everything local is dead. With maxFailoverAttempts=2 the request tries
	// hypatia, archimedes, hekaton and then gives up rather than serially
	// scanning the whole catalogue.
	tp := &deadBackends{dead: map[string]bool{
		"hypatia.local:5391":    true,
		"archimedes.local:5391": true,
		"192.168.42.20:5391":    true,
	}}
	rt, _ := newRoleRouter(t, nil, WithTransport(tp))

	rec := postTo(t, rt, "/v1/chat/completions", `{"model":"coder","messages":[]}`)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 once the attempt cap is hit", rec.Code)
	}
	if n := len(tp.dialed()); n != maxFailoverAttempts+1 {
		t.Errorf("dialed %d backends, want %d (initial + %d retries)", n, maxFailoverAttempts+1, maxFailoverAttempts)
	}
}

func TestDirectModelDoesNotFailOver(t *testing.T) {
	// A named model that is off must 502, not silently answer from a peer.
	tp := &deadBackends{dead: map[string]bool{"hypatia.local:5391": true}}
	rt, avail := newRoleRouter(t, nil, WithTransport(tp))

	rec := postTo(t, rt, "/v1/chat/completions", `{"model":"qwen36-hypatia","messages":[]}`)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for an explicitly named dead model", rec.Code)
	}
	if n := len(tp.dialed()); n != 1 {
		t.Errorf("dialed %d backends, want exactly 1 — direct names must not reassign", n)
	}
	// The failure is still reported: the tracker wants to know.
	if len(avail.failures) != 1 {
		t.Errorf("failures = %v, want the failure reported even without failover", avail.failures)
	}
	if got := rec.Header().Get("X-Router-Role"); got != "" {
		t.Errorf("X-Router-Role = %q, want empty for a direct model", got)
	}
}

// halfWrittenTransport answers with a 200 header and then fails mid-body,
// which is the case where retrying would corrupt what the client already has.
type halfWrittenTransport struct{ calls int }

func (h *halfWrittenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h.calls++
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(&failingReader{data: []byte("data: {\"choices\":[]}\n\n")}),
		Request:    req,
	}, nil
}

// failingReader emits its payload once, then errors — a stream that dies after
// the client has already seen bytes.
type failingReader struct {
	data []byte
	done bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if !f.done {
		f.done = true
		n := copy(p, f.data)
		return n, nil
	}
	return 0, errors.New("upstream vanished mid-stream")
}

func TestNoFailoverOnceBytesReachedTheClient(t *testing.T) {
	tp := &halfWrittenTransport{}
	rt, _ := newRoleRouter(t, nil, WithTransport(tp))

	rec := postTo(t, rt, "/v1/chat/completions", `{"model":"coder","messages":[]}`)
	if tp.calls != 1 {
		t.Errorf("upstream called %d times, want 1 — a partially written response must never be retried", tp.calls)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want the already-sent 200 preserved", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "data:") {
		t.Errorf("body %q should keep the bytes already delivered", rec.Body.String())
	}
}

func TestAvailabilityEndpoint(t *testing.T) {
	rt, _ := newRoleRouter(t, map[string]bool{"qwen36-hypatia": true})

	rec := getFrom(t, rt, "/v1/availability")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got availabilityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Mode != "default" {
		t.Errorf("mode = %q, want default", got.Mode)
	}
	byRole := map[string]roleBinding{}
	for _, b := range got.Roles {
		byRole[b.Role] = b
	}
	coder, ok := byRole["coder"]
	if !ok {
		t.Fatalf("coder role missing from %v", got.Roles)
	}
	if !coder.Available || coder.Target != "minimax-reap" {
		t.Errorf("coder = %+v, want available on minimax-reap", coder)
	}
	// The preference order is shown so an operator can see what's next.
	if len(coder.Candidates) == 0 {
		t.Errorf("coder should report its candidate list")
	}
}

func TestAvailabilityEndpointReportsUnavailableRole(t *testing.T) {
	rt, _ := newRoleRouter(t, map[string]bool{
		"minimax-reap":   true,
		"qwen36-hypatia": true,
	})
	rec := getFrom(t, rt, "/v1/availability")
	var got availabilityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, b := range got.Roles {
		if b.Role != "thinker" {
			continue
		}
		if b.Available {
			t.Errorf("thinker should be unavailable")
		}
		if len(b.Reasons) == 0 {
			t.Errorf("thinker should explain which candidates are out")
		}
		return
	}
	t.Fatalf("thinker role missing from response")
}
