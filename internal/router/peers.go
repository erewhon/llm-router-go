package router

// Peer feeds: the fleet-wide Activity view.
//
// Each replica's event broker (events.go) knows only the requests that
// replica handled, and the OVN load balancer hands the dashboard's browser
// to whichever replica it likes, so the Activity tab showed half the fleet.
// A peer feed is one goroutine per configured peer that holds that peer's
// GET /api/events stream open and mirrors it into a broker of its own: the
// peer's `snapshot` frame Resets the broker (ring + in-flight) on every
// (re)connect, and each `request` frame is published into it. The SSE
// handler then merges the local broker with every peer broker, so a page
// opened on either replica sees both — including the peer's recent past.
//
// The peer's events are kept in their own broker rather than re-published
// into the local one so that nothing this replica serves to ITS peers ever
// contains a peer's events: two replicas subscribed to each other cannot
// ping-pong, and a replica listed as its own peer just gets skipped.
//
// Auth: the peer's /api/events sits behind its identity gate. The feed
// sends the shared secret plus the first configured owner as the identity,
// so it receives everything; the viewer's own filter is applied after the
// merge, exactly as for local events. Every replica shares one proxy.env,
// so the secret and owners match by construction.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Reconnect backoff bounds; package vars so tests can shrink them.
var (
	peerBackoffMin = time.Second
	peerBackoffMax = 30 * time.Second
	// peerDialTimeout bounds the overview probe and the SSE connect; the
	// stream itself has no deadline (keepalives every 15 s prove liveness).
	peerDialTimeout = 5 * time.Second
	// peerStaleAfter is how long a silent stream is tolerated before it is
	// dropped and redialled: three missed keepalives.
	peerStaleAfter = 3*eventsKeepalive + 5*time.Second
)

// PeerState is one peer feed's health, for /api/overview and tests.
type PeerState struct {
	URL       string `json:"url"`
	Replica   string `json:"replica,omitempty"` // learned from the peer's /api/overview
	Connected bool   `json:"connected"`
	// Self is set when the peer's replica name is our own: the feed is
	// parked for good (with a same-list-everywhere config that is expected,
	// not an error).
	Self       bool      `json:"self,omitempty"`
	Reconnects int       `json:"reconnects"`
	LastEvent  time.Time `json:"last_event_ts,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
}

type peerFeed struct {
	url    string
	broker *Broker
	hdr    http.Header // auth headers sent on every call to the peer

	mu    sync.Mutex
	state PeerState
}

// peerSet is every configured peer feed plus what they share.
type peerSet struct {
	feeds  []*peerFeed
	self   string
	client *http.Client
	logger *slog.Logger

	// overview probe cache; see peerOverviews.
	ovMu sync.Mutex
	ov   []ReplicaOverview
	ovAt time.Time
}

func newPeerSet(rt *Router, cfg DashboardConfig) *peerSet {
	ps := &peerSet{
		self:   rt.replica,
		client: &http.Client{Transport: http.DefaultTransport},
		logger: rt.logger.With("subsys", "peers"),
	}
	hdr := http.Header{}
	if cfg.AuthSecret != "" {
		hdr.Set(DashboardAuthHeader, cfg.AuthSecret)
		idHdr := cfg.IdentityHeader
		if idHdr == "" {
			idHdr = "X-Auth-Request-Email"
		}
		if len(cfg.Owners) > 0 {
			hdr.Set(idHdr, cfg.Owners[0])
		} else {
			ps.logger.Warn("dashboard peers configured with an auth secret but no --dashboard-owners; peer feeds will be refused by the peers' identity gate")
		}
	}
	for _, u := range cfg.Peers {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u == "" {
			continue
		}
		ps.feeds = append(ps.feeds, &peerFeed{
			url:    u,
			broker: newBroker(DefaultEventRing),
			hdr:    hdr,
			state:  PeerState{URL: u},
		})
	}
	return ps
}

// RunPeerFeeds runs every configured peer feed until ctx ends. Call it once
// after DashboardHandler; with no peers it returns at once.
func (rt *Router) RunPeerFeeds(ctx context.Context) {
	ps := rt.peers
	if ps == nil || len(ps.feeds) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, f := range ps.feeds {
		wg.Add(1)
		go func(f *peerFeed) {
			defer wg.Done()
			ps.run(ctx, f)
		}(f)
	}
	wg.Wait()
}

// PeerStates reports every peer feed's health, in configured order.
func (rt *Router) PeerStates() []PeerState {
	if rt.peers == nil {
		return nil
	}
	out := make([]PeerState, 0, len(rt.peers.feeds))
	for _, f := range rt.peers.feeds {
		f.mu.Lock()
		out = append(out, f.state)
		f.mu.Unlock()
	}
	return out
}

// peerBrokers are the brokers of the peers that are not us, for the merge.
func (rt *Router) peerBrokers() []*Broker {
	if rt.peers == nil {
		return nil
	}
	var out []*Broker
	for _, f := range rt.peers.feeds {
		f.mu.Lock()
		self := f.state.Self
		f.mu.Unlock()
		if !self {
			out = append(out, f.broker)
		}
	}
	return out
}

func (f *peerFeed) update(fn func(*PeerState)) {
	f.mu.Lock()
	fn(&f.state)
	f.mu.Unlock()
}

// run is one feed's life: identify the peer, stream it, back off, repeat.
func (ps *peerSet) run(ctx context.Context, f *peerFeed) {
	backoff := peerBackoffMin
	loggedDown := false
	for {
		connected, err := ps.stream(ctx, f)
		if ctx.Err() != nil {
			return
		}
		f.mu.Lock()
		self := f.state.Self
		f.mu.Unlock()
		if self {
			ps.logger.Info("dashboard peer is this replica; feed parked", "peer", f.url, "replica", ps.self)
			return
		}
		f.update(func(s *PeerState) {
			s.Connected = false
			s.Reconnects++
			if err != nil {
				s.LastError = err.Error()
			}
		})
		f.broker.ClearInFlight()
		if connected {
			// A stream that was up resets the backoff; the loss is news.
			backoff = peerBackoffMin
			loggedDown = false
		}
		if !loggedDown {
			// Once per outage, not once per retry.
			ps.logger.Warn("dashboard peer feed down", "peer", f.url, "err", err)
			loggedDown = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, peerBackoffMax)
	}
}

// stream identifies the peer via /api/overview, then holds /api/events open
// and mirrors it until the connection ends. connected reports whether the
// stream got as far as a snapshot; err is nil only on ctx end or self.
func (ps *peerSet) stream(ctx context.Context, f *peerFeed) (connected bool, err error) {
	name, err := ps.identify(ctx, f)
	if err != nil {
		return false, err
	}
	if name != "" && name == ps.self {
		f.update(func(s *PeerState) { s.Replica = name; s.Self = true })
		return false, nil
	}
	f.update(func(s *PeerState) { s.Replica = name })

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url+"/api/events?scope=local", nil)
	if err != nil {
		return false, err
	}
	req.Header = f.hdr.Clone()
	req.Header.Set("Accept", "text/event-stream")
	resp, err := ps.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("peer /api/events: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	// A watchdog closes the body if the stream goes silent past the
	// keepalive budget, which unblocks the reader below.
	stallCtx, cancelStall := context.WithCancel(ctx)
	defer cancelStall()
	activity := make(chan struct{}, 1)
	go func() {
		t := time.NewTimer(peerStaleAfter)
		defer t.Stop()
		for {
			select {
			case <-stallCtx.Done():
				return
			case <-activity:
				if !t.Stop() {
					<-t.C
				}
				t.Reset(peerStaleAfter)
			case <-t.C:
				resp.Body.Close()
				return
			}
		}
	}()
	touch := func() {
		select {
		case activity <- struct{}{}:
		default:
		}
	}

	first := true
	r := bufio.NewReader(resp.Body)
	var event, data string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if ctx.Err() != nil {
				return connected, nil
			}
			if errors.Is(err, io.EOF) {
				return connected, errors.New("peer closed the event stream")
			}
			return connected, err
		}
		touch()
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if event == "" {
				continue // keepalive comment
			}
			if err := ps.apply(f, event, data); err != nil {
				ps.logger.Warn("dashboard peer sent an unreadable frame", "peer", f.url, "event", event, "err", err)
			} else if first {
				first, connected = false, true
				f.update(func(s *PeerState) { s.Connected = true; s.LastError = "" })
				ps.logger.Info("dashboard peer feed connected", "peer", f.url, "replica", name)
			}
			event, data = "", ""
		}
	}
}

// identify asks the peer's (ungated) /api/overview for its replica name.
// scope=local: the name is all that is wanted, and a fleet-wide answer would
// have the peer probe its own peers (us included) just to say who it is.
func (ps *peerSet) identify(ctx context.Context, f *peerFeed) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, peerDialTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url+"/api/overview?scope=local", nil)
	if err != nil {
		return "", err
	}
	req.Header = f.hdr.Clone()
	resp, err := ps.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("peer /api/overview: %s", resp.Status)
	}
	var ov struct {
		Replica string `json:"replica"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ov); err != nil {
		return "", fmt.Errorf("peer /api/overview: %w", err)
	}
	return ov.Replica, nil
}

// apply folds one SSE frame from the peer into its broker.
func (ps *peerSet) apply(f *peerFeed, event, data string) error {
	switch event {
	case "snapshot":
		var snap struct {
			Inflight []Event `json:"inflight"`
			Recent   []Event `json:"recent"`
		}
		if err := json.Unmarshal([]byte(data), &snap); err != nil {
			return err
		}
		f.broker.Reset(ps.own(f, snap.Recent), ps.own(f, snap.Inflight))
		f.update(func(s *PeerState) { s.LastEvent = time.Now() })
		return nil
	case "request":
		var e Event
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return err
		}
		own := ps.own(f, []Event{e})
		if len(own) == 0 {
			return nil
		}
		f.broker.Publish(own[0])
		f.update(func(s *PeerState) { s.LastEvent = own[0].TS })
		return nil
	}
	return nil // unknown frame types are ignored, for forward compatibility
}

// own keeps the events the peer handled itself: a Replica of its own name,
// or none (a peer too old to stamp, which also predates scope=local and so
// could not be mirroring anyone). Anything else is second-hand — ours, or a
// third replica's that we hear from directly — and is dropped.
func (ps *peerSet) own(f *peerFeed, evs []Event) []Event {
	f.mu.Lock()
	name := f.state.Replica
	f.mu.Unlock()
	out := evs[:0]
	for _, e := range evs {
		switch e.Replica {
		case "":
			e.Replica = name
		case name:
		default:
			continue
		}
		out = append(out, e)
	}
	return out
}

// ---------------------------------------------------------------------------
// Fleet-wide /api/overview
// ---------------------------------------------------------------------------

// ReplicaOverview is one replica's slice of the header strip: what
// /api/overview reports about itself, as seen from here.
type ReplicaOverview struct {
	Replica        string  `json:"replica"`
	URL            string  `json:"url,omitempty"` // peers only
	Version        string  `json:"version"`
	UptimeS        float64 `json:"uptime_s"`
	RequestsPerMin int     `json:"requests_per_min"`
	// Reachable is whether the peer answered /api/overview just now; Error
	// says why not. Feed is the event-feed side of the same peer, so a
	// replica that answers HTTP but whose stream is down still shows amber.
	Reachable bool       `json:"reachable"`
	Error     string     `json:"error,omitempty"`
	Feed      *PeerState `json:"feed,omitempty"` // peers only
}

// Peer overview probes are cached briefly so the strip's poll (every 10 s
// per open page) does not amplify into a peer call per page per poll.
var (
	peerOverviewTTL     = 2 * time.Second
	peerOverviewTimeout = time.Second
)

// peerOverviews probes every non-self peer's /api/overview?scope=local,
// concurrently, with a short cache. The scope=local is load-bearing: a
// peer's overview would otherwise probe its own peers, including us, and
// two replicas would chase each other until the timeouts fired.
func (ps *peerSet) peerOverviews(ctx context.Context) []ReplicaOverview {
	ps.ovMu.Lock()
	if time.Since(ps.ovAt) < peerOverviewTTL && ps.ov != nil {
		out := ps.ov
		ps.ovMu.Unlock()
		return out
	}
	ps.ovMu.Unlock()

	type slot struct {
		i  int
		ov ReplicaOverview
	}
	var feeds []*peerFeed
	for _, f := range ps.feeds {
		f.mu.Lock()
		self := f.state.Self
		f.mu.Unlock()
		if !self {
			feeds = append(feeds, f)
		}
	}
	results := make(chan slot, len(feeds))
	for i, f := range feeds {
		go func(i int, f *peerFeed) {
			results <- slot{i, ps.probeOverview(ctx, f)}
		}(i, f)
	}
	out := make([]ReplicaOverview, len(feeds))
	for range feeds {
		s := <-results
		out[s.i] = s.ov
	}
	ps.ovMu.Lock()
	ps.ov, ps.ovAt = out, time.Now()
	ps.ovMu.Unlock()
	return out
}

func (ps *peerSet) probeOverview(ctx context.Context, f *peerFeed) ReplicaOverview {
	f.mu.Lock()
	state := f.state
	f.mu.Unlock()
	ov := ReplicaOverview{Replica: state.Replica, URL: f.url, Feed: &state}
	ctx, cancel := context.WithTimeout(ctx, peerOverviewTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url+"/api/overview?scope=local", nil)
	if err != nil {
		ov.Error = err.Error()
		return ov
	}
	req.Header = f.hdr.Clone()
	resp, err := ps.client.Do(req)
	if err != nil {
		ov.Error = err.Error()
		return ov
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		ov.Error = "peer /api/overview: " + resp.Status
		return ov
	}
	var body struct {
		Replica        string  `json:"replica"`
		Version        string  `json:"version"`
		UptimeS        float64 `json:"uptime_s"`
		RequestsPerMin int     `json:"requests_per_min"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		ov.Error = "peer /api/overview: " + err.Error()
		return ov
	}
	if body.Replica != "" {
		ov.Replica = body.Replica
	}
	ov.Version, ov.UptimeS, ov.RequestsPerMin, ov.Reachable = body.Version, body.UptimeS, body.RequestsPerMin, true
	return ov
}
