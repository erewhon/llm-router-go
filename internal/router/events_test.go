package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erewhon/llm-router-go/internal/httpx"
)

// gatedUpstream answers only after release() is called, so a request can be
// observed in flight.
type gatedUpstream struct {
	release chan struct{}
	once    sync.Once
}

func newGatedUpstream() *gatedUpstream { return &gatedUpstream{release: make(chan struct{})} }
func (g *gatedUpstream) open()         { g.once.Do(func() { close(g.release) }) }
func (g *gatedUpstream) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case <-g.release:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"x","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`)),
		Request:    req,
	}, nil
}

// withRequestIDs wraps the router the way main does, so events carry ids.
func withRequestIDs(rt *Router) http.Handler { return httpx.Chain(rt.Handler(), httpx.RequestID) }

func collect(ch <-chan Event, n int, timeout time.Duration) []Event {
	var out []Event
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case e, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, e)
		case <-deadline:
			return out
		}
	}
	return out
}

func TestEvents_StartedThenFinishedForAProxiedRequest(t *testing.T) {
	up := newGatedUpstream()
	rt := newTestRouter(t, up)
	ch, cancel := rt.Events().Subscribe(16)
	defer cancel()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"qwen36-hypatia","messages":[{"role":"user","content":"hi"}],"stream":false}`))
		req.Header.Set("Content-Type", "application/json")
		withRequestIDs(rt).ServeHTTP(rec, req)
		done <- rec
	}()

	started := collect(ch, 1, 2*time.Second)
	if len(started) != 1 || started[0].Type != EventStarted {
		t.Fatalf("want a started event first, got %+v", started)
	}
	s := started[0]
	if s.RequestID == "" || s.ResolvedVia != "qwen36-hypatia" || s.Node != "hypatia" || s.Model != "qwen36-hypatia" {
		t.Errorf("started = %+v", s)
	}
	if inflight := rt.Events().InFlight(); len(inflight) != 1 || inflight[0].RequestID != s.RequestID {
		t.Errorf("InFlight while blocked = %+v, want the one request", inflight)
	}

	up.open()
	rec := <-done
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	fin := collect(ch, 1, 2*time.Second)
	if len(fin) != 1 || fin[0].Type != EventFinished {
		t.Fatalf("want a finished event, got %+v", fin)
	}
	f := fin[0]
	if f.RequestID != s.RequestID || f.ResolvedVia != "qwen36-hypatia" || f.Status != 200 || f.LatencyMS < 0 {
		t.Errorf("finished = %+v", f)
	}
	if f.PromptTokens == nil || *f.PromptTokens != 3 || f.CompletionTokens == nil || *f.CompletionTokens != 4 {
		t.Errorf("finished tokens = %v/%v, want 3/4", f.PromptTokens, f.CompletionTokens)
	}
	if len(rt.Events().InFlight()) != 0 {
		t.Errorf("InFlight after finish should be empty")
	}
	if rp := rt.Events().Replay(); len(rp) != 2 || rp[0].Type != EventStarted || rp[1].Type != EventFinished {
		t.Errorf("Replay = %+v, want started then finished", rp)
	}
}

func TestEvents_FailoverPublishesASecondStart(t *testing.T) {
	tp := &deadBackends{dead: map[string]bool{"hypatia.local:5391": true}}
	rt, _ := newRoleRouter(t, nil, WithTransport(tp))
	ch, cancel := rt.Events().Subscribe(16)
	defer cancel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coder","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	withRequestIDs(rt).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	evs := collect(ch, 3, 2*time.Second)
	if len(evs) != 3 {
		t.Fatalf("events = %d, want started, started(failover), finished: %+v", len(evs), evs)
	}
	if evs[0].Type != EventStarted || evs[0].ResolvedVia != "qwen36-hypatia" || evs[0].Role != "coder" {
		t.Errorf("first = %+v", evs[0])
	}
	if evs[1].Type != EventStarted || evs[1].ResolvedVia != "minimax-reap" || evs[1].FailoverFrom != "qwen36-hypatia" {
		t.Errorf("second = %+v, want a started for minimax-reap with failover_from", evs[1])
	}
	if evs[2].Type != EventFinished || evs[2].ResolvedVia != "minimax-reap" || evs[2].FailoverFrom != "qwen36-hypatia" {
		t.Errorf("finished = %+v", evs[2])
	}
	if evs[0].RequestID == "" || evs[0].RequestID != evs[1].RequestID || evs[1].RequestID != evs[2].RequestID {
		t.Errorf("request ids should match across the hops: %q %q %q", evs[0].RequestID, evs[1].RequestID, evs[2].RequestID)
	}
}

func TestEvents_RejectedRequestOnlyFinishes(t *testing.T) {
	rt := newTestRouter(t, nil)
	ch, cancel := rt.Events().Subscribe(16)
	defer cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"no-such","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	withRequestIDs(rt).ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("status = %d", rec.Code)
	}
	evs := collect(ch, 2, 500*time.Millisecond)
	if len(evs) != 1 || evs[0].Type != EventFinished || evs[0].Status != 404 || evs[0].ResolvedVia != "" {
		t.Errorf("events = %+v, want exactly one finished 404 with no target", evs)
	}
	if len(rt.Events().InFlight()) != 0 {
		t.Errorf("nothing should be in flight")
	}
}

func TestEvents_SlowSubscriberDropsNotBlocks(t *testing.T) {
	b := newBroker(8)
	_, cancel := b.Subscribe(1)
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			b.Publish(Event{Type: EventFinished, RequestID: "r"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full subscriber")
	}
	if d := b.Dropped(); d != 9 {
		t.Errorf("dropped = %d, want 9 (1 buffered)", d)
	}
	// The ring holds at most its capacity, oldest first.
	if rp := b.Replay(); len(rp) != 8 {
		t.Errorf("Replay = %d events, want the ring size 8", len(rp))
	}
	b2 := newBroker(3)
	for i := 1; i <= 5; i++ {
		b2.Publish(Event{Type: EventStarted, RequestID: string(rune('0' + i)), TS: time.Unix(int64(i), 0)})
	}
	rp := b2.Replay()
	if len(rp) != 3 || rp[0].RequestID != "3" || rp[2].RequestID != "5" {
		t.Errorf("Replay after wrap = %+v, want 3,4,5", rp)
	}
	// Cancel closes the channel and stops delivery.
	ch, c := b.Subscribe(4)
	c()
	if _, ok := <-ch; ok {
		t.Error("channel should be closed after cancel")
	}
	c() // idempotent
	_ = context.Background()
}
