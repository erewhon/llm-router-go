package router

// GET /api/events — the request-event broker as server-sent events, for the
// dashboard's Activity view. On connect the client gets one `snapshot` frame
// (what is in flight plus the replay ring), then one `request` frame per
// live event, with a keepalive comment every 15 s so proxies keep the
// connection open. Events carry principals, so the route sits behind the
// same identity gate as /api/usage: with a --dashboard-auth-secret only a
// proxy-verified identity gets it, and a non-owner sees only their own
// requests; without one (local dev) it is open, like /api/chat.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/erewhon/llm-router-go/internal/auth"
)

// eventsKeepalive is how often a comment frame goes out on an idle stream.
var eventsKeepalive = 15 * time.Second

func (rt *Router) handleDashEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeDashError(w, http.StatusInternalServerError, "streaming unsupported on this listener")
		return
	}
	// Who is asking decides what they see. No identity (gate off) = owner.
	allow := func(Event) bool { return true }
	if id, ok := auth.FromContext(r.Context()); ok && !isOwner(id) {
		mine := id.Principal
		allow = func(e Event) bool { return e.Principal == mine }
	}
	filter := func(in []Event) []Event {
		out := make([]Event, 0, len(in))
		for _, e := range in {
			if allow(e) {
				out = append(out, e)
			}
		}
		return out
	}

	// Subscribe before the snapshot so nothing published in between is lost
	// (a duplicate between ring and stream is harmless; a gap is not).
	ch, cancel := rt.events.Subscribe(64)
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	snap, _ := json.Marshal(map[string]any{
		"inflight": filter(rt.events.InFlight()),
		"recent":   filter(rt.events.Replay()),
	})
	fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", snap)
	flusher.Flush()

	tick := time.NewTicker(eventsKeepalive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			if !allow(e) {
				continue
			}
			data, err := json.Marshal(e)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: request\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}
