package router

// Request events: the live feed behind the dashboard's Activity view.
//
// A reqlog record is written when a request ENDS, which is fine for history
// and useless for a picture of what is happening now. This broker publishes a
// `started` event the moment a request has resolved to a target (before the
// upstream is called) and a `finished` event from the same deferred block that
// writes the reqlog record, so the two always agree. A failover mid-request
// publishes a second `started` for the new target with FailoverFrom set, so a
// view can draw the second hop.
//
// It is on the request path, so: one mutex, no allocation beyond the event,
// and a subscriber that does not keep up drops events rather than stalling
// Publish. The ring lets a page that connects late draw the recent past.

import (
	"net/url"
	"sync"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
)

// EventType is "started" or "finished".
type EventType string

const (
	EventStarted  EventType = "started"
	EventFinished EventType = "finished"
)

// Event is one request lifecycle transition. The JSON names are the wire
// contract for /api/events and the Activity tab.
type Event struct {
	Type      EventType `json:"type"`
	RequestID string    `json:"request_id"`
	TS        time.Time `json:"ts"`
	Principal string    `json:"principal,omitempty"`
	// Model is the name the caller sent; ResolvedVia the registry id that
	// serves it; Role/Chain the handle it came through, if any.
	Model       string `json:"model"`
	ResolvedVia string `json:"resolved_via,omitempty"`
	Role        string `json:"role,omitempty"`
	Chain       string `json:"chain,omitempty"`
	// Node is the fleet node of the resolved model ("" for externals);
	// Provider is the api_base host of an external ("" for local). One of the
	// two is set for every resolved request, which is what lets the view put
	// the dot somewhere.
	Node       string `json:"node,omitempty"`
	Provider   string `json:"provider,omitempty"`
	BackendURL string `json:"backend_url,omitempty"`
	Stream     bool   `json:"stream"`
	Discovered bool   `json:"discovered,omitempty"`
	// finished only.
	Status           int    `json:"status,omitempty"`
	LatencyMS        int    `json:"latency_ms,omitempty"`
	PromptTokens     *int   `json:"prompt_tokens,omitempty"`
	CompletionTokens *int   `json:"completion_tokens,omitempty"`
	FailoverFrom     string `json:"failover_from,omitempty"`
	Overflowed       bool   `json:"overflowed,omitempty"`
	PrivacyTolerance string `json:"privacy_tolerance,omitempty"`
	ErrorClass       string `json:"error_class,omitempty"`
	UpstreamProvider string `json:"upstream_provider,omitempty"`
}

// Broker fans events out to subscribers and keeps a replay ring.
type Broker struct {
	mu       sync.Mutex
	ring     []Event // capacity fixed at construction; oldest first after wrap
	head     int     // next write position
	full     bool
	inflight map[string]Event // request_id → latest started event
	subs     map[*subscriber]struct{}
}

type subscriber struct {
	ch      chan Event
	dropped int
}

// DefaultEventRing is how many events a late-connecting page can replay.
const DefaultEventRing = 500

func newBroker(ring int) *Broker {
	if ring <= 0 {
		ring = DefaultEventRing
	}
	return &Broker{
		ring:     make([]Event, ring),
		inflight: map[string]Event{},
		subs:     map[*subscriber]struct{}{},
	}
}

// Publish records the event and hands it to every subscriber without ever
// blocking: a full subscriber channel drops the event and counts the drop.
func (b *Broker) Publish(e Event) {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	b.mu.Lock()
	b.ring[b.head] = e
	b.head = (b.head + 1) % len(b.ring)
	if b.head == 0 {
		b.full = true
	}
	if e.RequestID != "" {
		switch e.Type {
		case EventStarted:
			b.inflight[e.RequestID] = e
		case EventFinished:
			delete(b.inflight, e.RequestID)
		}
	}
	for s := range b.subs {
		select {
		case s.ch <- e:
		default:
			s.dropped++
		}
	}
	b.mu.Unlock()
}

// Subscribe returns a channel of live events and a cancel function that
// closes it. buf is the channel depth; events beyond it are dropped for this
// subscriber only.
func (b *Broker) Subscribe(buf int) (<-chan Event, func()) {
	if buf <= 0 {
		buf = 64
	}
	s := &subscriber{ch: make(chan Event, buf)}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, s)
			b.mu.Unlock()
			close(s.ch)
		})
	}
	return s.ch, cancel
}

// Dropped reports how many events were dropped across all subscribers,
// for tests and a future metric.
func (b *Broker) Dropped() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for s := range b.subs {
		n += s.dropped
	}
	return n
}

// Replay returns the ring's contents, oldest first.
func (b *Broker) Replay() []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.full {
		return append([]Event(nil), b.ring[:b.head]...)
	}
	out := make([]Event, 0, len(b.ring))
	out = append(out, b.ring[b.head:]...)
	out = append(out, b.ring[:b.head]...)
	return out
}

// InFlight returns the started events with no matching finished yet, in
// start order.
func (b *Broker) InFlight() []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Event, 0, len(b.inflight))
	for _, e := range b.inflight {
		out = append(out, e)
	}
	// Oldest first: stable for the view and for tests.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].TS.Before(out[j-1].TS); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Events returns the router's broker, for the dashboard's SSE endpoint.
func (rt *Router) Events() *Broker { return rt.events }

// eventPlacement fills Node/Provider/BackendURL/Discovered for a resolved
// target: the fleet node for a local seat, the api_base host for an
// external.
func (rt *Router) eventPlacement(e *Event, res resolveResult) {
	e.ResolvedVia = res.ModelID
	e.Role = res.Role
	e.Chain = res.Chain
	e.BackendURL = res.BackendURL
	e.Discovered = res.Discovered
	if m, ok := rt.lookupModel(res.ModelID); ok {
		e.Node = placementNode(m)
		if e.Node == "" && m.APIBase != "" {
			if u, err := url.Parse(m.APIBase); err == nil {
				e.Provider = u.Host
			}
		}
	}
}

func placementNode(m config.ModelDefinition) string {
	if m.MultiNode != nil {
		if m.MultiNode.HeadNode != "" {
			return m.MultiNode.HeadNode
		}
		if len(m.MultiNode.Nodes) > 0 {
			return m.MultiNode.Nodes[0]
		}
	}
	return m.Node
}
