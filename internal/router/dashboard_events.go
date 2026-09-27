package router

// GET /api/events — the request-event broker as server-sent events, for the
// dashboard's Activity view. On connect the client gets one `snapshot` frame
// (what is in flight plus the replay ring), then one `request` frame per
// live event, with a keepalive comment every 15 s so proxies keep the
// connection open. With peers configured (peers.go) the stream is the merge
// of this replica's broker and every peer's, so the view is fleet-wide from
// whichever replica the load balancer picked. Events carry principals, so the route sits behind the
// same identity gate as /api/usage: with a --dashboard-auth-secret only a
// proxy-verified identity gets it, and a non-owner sees only their own
// requests; without one (local dev) it is open, like /api/chat.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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
	// (a duplicate between ring and stream is harmless; a gap is not). A
	// peer feed asks for scope=local: it wants only what THIS replica
	// handled, never what this replica mirrored from someone (possibly the
	// asker) — that is what keeps two replicas from echoing each other.
	brokers := []*Broker{rt.events}
	if r.URL.Query().Get("scope") != "local" {
		brokers = append(brokers, rt.peerBrokers()...)
	}
	ch, cancel := subscribeAll(brokers, 64)
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var inflight, recent []Event
	for _, b := range brokers {
		inflight = append(inflight, b.InFlight()...)
		recent = append(recent, b.Replay()...)
	}
	byTS := func(evs []Event) { sort.SliceStable(evs, func(i, j int) bool { return evs[i].TS.Before(evs[j].TS) }) }
	byTS(inflight)
	byTS(recent)
	if len(recent) > DefaultEventRing {
		recent = recent[len(recent)-DefaultEventRing:]
	}
	snap, _ := json.Marshal(map[string]any{
		"inflight": filter(inflight),
		"recent":   filter(recent),
	})
	fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", snap)
	flusher.Flush()

	tick := time.NewTicker(eventsKeepalive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-rt.streamsDone:
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

// subscribeAll fans several brokers' live streams into one channel. The
// single-broker case is the plain subscription; more than one gets a
// forwarding goroutine each, all torn down by the returned cancel.
func subscribeAll(brokers []*Broker, buf int) (<-chan Event, func()) {
	if len(brokers) == 1 {
		return brokers[0].Subscribe(buf)
	}
	out := make(chan Event, buf)
	done := make(chan struct{})
	cancels := make([]func(), 0, len(brokers))
	for _, b := range brokers {
		ch, cancel := b.Subscribe(buf)
		cancels = append(cancels, cancel)
		go func(ch <-chan Event) {
			for {
				select {
				case <-done:
					return
				case e, ok := <-ch:
					if !ok {
						return
					}
					select {
					case out <- e:
					case <-done:
						return
					}
				}
			}
		}(ch)
	}
	return out, func() {
		close(done)
		for _, c := range cancels {
			c()
		}
	}
}
