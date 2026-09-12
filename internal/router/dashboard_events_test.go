package router

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseStream turns one open event stream into frames. One reader goroutine
// per stream for its whole life — a second reader on the same body would
// race it and swallow frames.
type sseFrame struct{ event, data string }

type sseStream struct{ lines chan string }

func newSSEStream(body io.Reader) *sseStream {
	s := &sseStream{lines: make(chan string, 64)}
	r := bufio.NewReader(body)
	go func() {
		defer close(s.lines)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			s.lines <- strings.TrimRight(line, "\n")
		}
	}()
	return s
}

// frames returns up to n frames, stopping early when ctx ends.
func (s *sseStream) frames(ctx context.Context, n int) []sseFrame {
	var out []sseFrame
	cur := sseFrame{}
	for len(out) < n {
		select {
		case <-ctx.Done():
			return out
		case line, ok := <-s.lines:
			if !ok {
				return out
			}
			switch {
			case strings.HasPrefix(line, "event: "):
				cur.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.data = strings.TrimPrefix(line, "data: ")
			case line == "" && cur.event != "":
				out = append(out, cur)
				cur = sseFrame{}
			}
		}
	}
	return out
}

func eventsServer(t *testing.T, rt *Router, cfg DashboardConfig) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(rt.DashboardHandler(cfg))
	t.Cleanup(srv.Close)
	return srv
}

func openEvents(t *testing.T, ctx context.Context, url string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/events", nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	return resp
}

func TestDashboardEvents_GateAndSnapshot(t *testing.T) {
	rt := newTestRouter(t, nil)
	srv := eventsServer(t, rt, DashboardConfig{AuthSecret: "s3cret", Owners: []string{"owner@example"}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// No secret → the gate refuses, exactly as /api/usage does.
	if resp := openEvents(t, ctx, srv.URL, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without the proxy secret: %d, want 401", resp.StatusCode)
	}
	// Valid identity → an event stream whose first frame is the snapshot.
	resp := openEvents(t, ctx, srv.URL, map[string]string{DashboardAuthHeader: "s3cret", "X-Auth-Request-Email": "owner@example"})
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status/content-type = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	frames := newSSEStream(resp.Body).frames(ctx, 1)
	if len(frames) != 1 || frames[0].event != "snapshot" {
		t.Fatalf("first frame = %+v, want snapshot", frames)
	}
	var snap struct {
		Inflight []Event `json:"inflight"`
		Recent   []Event `json:"recent"`
	}
	if err := json.Unmarshal([]byte(frames[0].data), &snap); err != nil {
		t.Fatalf("snapshot json: %v", err)
	}
	if len(snap.Inflight) != 0 || len(snap.Recent) != 0 {
		t.Errorf("fresh router should have an empty snapshot: %+v", snap)
	}
}

func TestDashboardEvents_StreamsARequestAndFiltersByPrincipal(t *testing.T) {
	rt := newTestRouter(t, nil)
	srv := eventsServer(t, rt, DashboardConfig{AuthSecret: "s3cret", Owners: []string{"owner@example"}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	owner := openEvents(t, ctx, srv.URL, map[string]string{DashboardAuthHeader: "s3cret", "X-Auth-Request-Email": "owner@example"})
	defer owner.Body.Close()
	other := openEvents(t, ctx, srv.URL, map[string]string{DashboardAuthHeader: "s3cret", "X-Auth-Request-Email": "family@example"})
	defer other.Body.Close()
	ownerR, otherR := newSSEStream(owner.Body), newSSEStream(other.Body)
	ownerR.frames(ctx, 1) // snapshots
	otherR.frames(ctx, 1)

	// Two unattributed 404s (no principal on a test request) — the owner sees
	// both frames per request; the non-owner sees none of them.
	for i := 0; i < 2; i++ {
		postChat(t, rt, `{"model":"no-such","messages":[]}`)
	}
	got := ownerR.frames(ctx, 2)
	if len(got) != 2 || got[0].event != "request" {
		t.Fatalf("owner frames = %+v, want two request frames", got)
	}
	var e Event
	_ = json.Unmarshal([]byte(got[0].data), &e)
	if e.Type != EventFinished || e.Status != 404 {
		t.Errorf("owner event = %+v, want a finished 404", e)
	}
	shortCtx, c2 := context.WithTimeout(ctx, 400*time.Millisecond)
	defer c2()
	if leaked := otherR.frames(shortCtx, 1); len(leaked) != 0 {
		t.Errorf("non-owner received another principal's event: %+v", leaked)
	}
	// An event carrying the non-owner's own principal reaches them.
	rt.Events().Publish(Event{Type: EventFinished, RequestID: "own", Principal: "family@example", Model: "coder", Status: 200})
	mine := otherR.frames(ctx, 1)
	if len(mine) != 1 || !strings.Contains(mine[0].data, `"principal":"family@example"`) {
		t.Errorf("non-owner should receive their own event, got %+v", mine)
	}
}

func TestDashboardEvents_ClientGoneEndsTheHandler(t *testing.T) {
	rt := newTestRouter(t, nil)
	srv := eventsServer(t, rt, DashboardConfig{}) // no secret: open, like local dev
	ctx, cancel := context.WithCancel(context.Background())
	resp := openEvents(t, ctx, srv.URL, nil)
	newSSEStream(resp.Body).frames(ctx, 1)
	rt.events.mu.Lock()
	before := len(rt.events.subs)
	rt.events.mu.Unlock()
	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rt.events.mu.Lock()
		n := len(rt.events.subs)
		rt.events.mu.Unlock()
		if n < before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("subscriber not released after the client went away (subs=%d)", before)
}
