package router

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A pair of replicas, each configured with the other (and, if asked, itself)
// as a dashboard peer, on httptest servers whose URLs are known before the
// handlers exist.
type peerPair struct {
	a, b       *Router
	srvA, srvB *httptest.Server
	cancel     context.CancelFunc
	// bProbes counts overview probes of B, for the cache test. The feed's
	// identify call is one of them, which is why the test reads a baseline.
	bProbes atomic.Int64
}

const (
	peerSecret = "s3cret"
	peerOwner  = "owner@example"
)

var peerOwnerHdr = map[string]string{DashboardAuthHeader: peerSecret, "X-Auth-Request-Email": peerOwner}

func fastPeerBackoff(t *testing.T) {
	t.Helper()
	oldMin, oldMax := peerBackoffMin, peerBackoffMax
	peerBackoffMin, peerBackoffMax = 20*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { peerBackoffMin, peerBackoffMax = oldMin, oldMax })
}

func newPeerPair(t *testing.T, includeSelf bool, optsB ...Option) *peerPair {
	t.Helper()
	fastPeerBackoff(t)
	p := &peerPair{
		a:    newTestRouter(t, nil, WithReplica("replica-a")),
		b:    newTestRouter(t, nil, append([]Option{WithReplica("replica-b")}, optsB...)...),
		srvA: httptest.NewUnstartedServer(nil),
		srvB: httptest.NewUnstartedServer(nil),
	}
	urlA := "http://" + p.srvA.Listener.Addr().String()
	urlB := "http://" + p.srvB.Listener.Addr().String()
	cfg := func(peers ...string) DashboardConfig {
		return DashboardConfig{AuthSecret: peerSecret, Owners: []string{peerOwner}, Peers: peers}
	}
	peersA, peersB := []string{urlB}, []string{urlA}
	if includeSelf {
		peersA, peersB = []string{urlA, urlB}, []string{urlA, urlB}
	}
	p.srvA.Config.Handler = p.a.DashboardHandler(cfg(peersA...))
	bHandler := p.b.DashboardHandler(cfg(peersB...))
	p.srvB.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/overview" && r.URL.Query().Get("scope") == "local" {
			p.bProbes.Add(1)
		}
		bHandler.ServeHTTP(w, r)
	})
	p.srvA.Start()
	p.srvB.Start()
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go p.a.RunPeerFeeds(ctx)
	go p.b.RunPeerFeeds(ctx)
	t.Cleanup(func() {
		cancel()
		p.srvA.Close()
		p.srvB.Close()
	})
	return p
}

// waitPeer polls a router's peer states until pred holds for the peer at url.
func waitPeer(t *testing.T, rt *Router, url string, what string, pred func(PeerState) bool) PeerState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range rt.PeerStates() {
			if s.URL == url && pred(s) {
				return s
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("peer %s never became %s: %+v", url, what, rt.PeerStates())
	return PeerState{}
}

func connected(s PeerState) bool { return s.Connected }

func decodeEvents(t *testing.T, frames []sseFrame) []Event {
	t.Helper()
	out := make([]Event, 0, len(frames))
	for _, f := range frames {
		var e Event
		if err := json.Unmarshal([]byte(f.data), &e); err != nil {
			t.Fatalf("frame %q: %v", f.data, err)
		}
		out = append(out, e)
	}
	return out
}

func TestPeers_EventsMergeBothWaysWithoutDuplicates(t *testing.T) {
	p := newPeerPair(t, false)
	waitPeer(t, p.a, p.srvB.URL, "connected", connected)
	waitPeer(t, p.b, p.srvA.URL, "connected", connected)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	onA := openEvents(t, ctx, p.srvA.URL, peerOwnerHdr)
	defer onA.Body.Close()
	onB := openEvents(t, ctx, p.srvB.URL, peerOwnerHdr)
	defer onB.Body.Close()
	streamA, streamB := newSSEStream(onA.Body), newSSEStream(onB.Body)
	streamA.frames(ctx, 1) // snapshots
	streamB.frames(ctx, 1)

	p.a.Events().Publish(Event{Type: EventStarted, RequestID: "from-a", Principal: peerOwner, Model: "coder"})
	p.b.Events().Publish(Event{Type: EventStarted, RequestID: "from-b", Principal: peerOwner, Model: "coder"})

	for name, s := range map[string]*sseStream{"A": streamA, "B": streamB} {
		got := decodeEvents(t, s.frames(ctx, 2))
		seen := map[string]string{}
		for _, e := range got {
			seen[e.RequestID] = e.Replica
		}
		if seen["from-a"] != "replica-a" || seen["from-b"] != "replica-b" {
			t.Errorf("stream on %s saw %v, want from-a@replica-a and from-b@replica-b", name, seen)
		}
		short, c := context.WithTimeout(ctx, 300*time.Millisecond)
		if extra := s.frames(short, 1); len(extra) != 0 {
			t.Errorf("stream on %s got a duplicate: %+v", name, extra)
		}
		c()
	}

	// A page that opens late on A gets B's event in the snapshot: in flight
	// (no finished yet) and in the ring.
	late := openEvents(t, ctx, p.srvA.URL, peerOwnerHdr)
	defer late.Body.Close()
	frames := newSSEStream(late.Body).frames(ctx, 1)
	var snap struct{ Inflight, Recent []Event }
	if err := json.Unmarshal([]byte(frames[0].data), &snap); err != nil {
		t.Fatal(err)
	}
	ids := func(evs []Event) string {
		var s []string
		for _, e := range evs {
			s = append(s, e.RequestID+"@"+e.Replica)
		}
		return strings.Join(s, ",")
	}
	if got := ids(snap.Inflight); !strings.Contains(got, "from-b@replica-b") || !strings.Contains(got, "from-a@replica-a") {
		t.Errorf("late snapshot inflight = %s, want both replicas", got)
	}
	if got := ids(snap.Recent); !strings.Contains(got, "from-b@replica-b") {
		t.Errorf("late snapshot recent = %s, want B's event", got)
	}
	// The local broker itself never absorbs a peer's events (no ping-pong),
	// and scope=local — what a peer feed asks for — serves only that.
	for _, e := range p.a.Events().Replay() {
		if e.Replica != "replica-a" {
			t.Errorf("A's own broker holds a peer event: %+v", e)
		}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.srvA.URL+"/api/events?scope=local", nil)
	for k, v := range peerOwnerHdr {
		req.Header.Set(k, v)
	}
	local, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Body.Close()
	if data := newSSEStream(local.Body).frames(ctx, 1)[0].data; strings.Contains(data, "replica-b") {
		t.Errorf("scope=local snapshot on A includes B's events: %s", data)
	}
}

func TestPeers_SelfInTheListIsParked(t *testing.T) {
	p := newPeerPair(t, true)
	self := waitPeer(t, p.a, p.srvA.URL, "self", func(s PeerState) bool { return s.Self })
	if self.Replica != "replica-a" || self.Connected {
		t.Errorf("self peer state = %+v", self)
	}
	waitPeer(t, p.a, p.srvB.URL, "connected", connected)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	onA := openEvents(t, ctx, p.srvA.URL, peerOwnerHdr)
	defer onA.Body.Close()
	stream := newSSEStream(onA.Body)
	stream.frames(ctx, 1)
	p.a.Events().Publish(Event{Type: EventFinished, RequestID: "local", Principal: peerOwner, Model: "coder", Status: 200})
	if got := stream.frames(ctx, 1); len(got) != 1 {
		t.Fatalf("frames = %+v", got)
	}
	short, c := context.WithTimeout(ctx, 300*time.Millisecond)
	defer c()
	if extra := stream.frames(short, 1); len(extra) != 0 {
		t.Errorf("own event came back through the self peer: %+v", extra)
	}
}

func TestPeers_NonOwnerSeesOnlyTheirOwnAcrossReplicas(t *testing.T) {
	p := newPeerPair(t, false)
	waitPeer(t, p.b, p.srvA.URL, "connected", connected)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	family := openEvents(t, ctx, p.srvB.URL, map[string]string{DashboardAuthHeader: peerSecret, "X-Auth-Request-Email": "family@example"})
	defer family.Body.Close()
	stream := newSSEStream(family.Body)
	stream.frames(ctx, 1)

	p.a.Events().Publish(Event{Type: EventFinished, RequestID: "owners", Principal: peerOwner, Model: "coder", Status: 200})
	p.a.Events().Publish(Event{Type: EventFinished, RequestID: "theirs", Principal: "family@example", Model: "coder", Status: 200})
	got := decodeEvents(t, stream.frames(ctx, 1))
	if len(got) != 1 || got[0].RequestID != "theirs" || got[0].Replica != "replica-a" {
		t.Fatalf("non-owner on B got %+v, want only their own event from A", got)
	}
	short, c := context.WithTimeout(ctx, 300*time.Millisecond)
	defer c()
	if leaked := stream.frames(short, 1); len(leaked) != 0 {
		t.Errorf("non-owner received another principal's peer event: %+v", leaked)
	}
}

func TestPeers_LossClearsInflightAndReconnectFoldsTheSnapshot(t *testing.T) {
	p := newPeerPair(t, false)
	waitPeer(t, p.a, p.srvB.URL, "connected", connected)
	p.b.Events().Publish(Event{Type: EventStarted, RequestID: "hanging", Principal: peerOwner, Model: "coder"})

	// B goes away: A's feed reports it and forgets B's in-flight requests.
	// Listener first, or A's fast reconnect lands a fresh never-ending SSE
	// request between the connection sweep and Close, which then waits on
	// it forever.
	addr := p.srvB.Listener.Addr().String()
	p.srvB.Listener.Close()
	p.srvB.CloseClientConnections()
	p.srvB.Close()
	down := waitPeer(t, p.a, p.srvB.URL, "disconnected", func(s PeerState) bool { return !s.Connected && s.Reconnects > 0 })
	if down.LastError == "" {
		t.Errorf("disconnected state carries no error: %+v", down)
	}
	if n := len(p.a.peers.feeds[0].broker.InFlight()); n != 0 {
		t.Errorf("B's in-flight set should be cleared on loss, has %d", n)
	}

	// B comes back on the same address with history A never streamed; A's
	// reconnect takes the snapshot, so a late page on A sees it.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("could not re-listen on %s: %v", addr, err)
	}
	b2 := newTestRouter(t, nil, WithReplica("replica-b"))
	b2.Events().Publish(Event{Type: EventFinished, RequestID: "while-away", Principal: peerOwner, Model: "coder", Status: 200})
	srvB2 := &httptest.Server{Listener: ln, Config: &http.Server{Handler: b2.DashboardHandler(DashboardConfig{AuthSecret: peerSecret, Owners: []string{peerOwner}})}}
	srvB2.Start()
	// A's feed holds an SSE stream open on srvB2; stop the feeds before
	// closing it, or Close waits on that stream forever.
	t.Cleanup(func() { p.cancel(); srvB2.Close() })
	up := waitPeer(t, p.a, p.srvB.URL, "reconnected", connected)
	if up.LastError != "" {
		t.Errorf("reconnected state still carries an error: %+v", up)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	late := openEvents(t, ctx, p.srvA.URL, peerOwnerHdr)
	defer late.Body.Close()
	frames := newSSEStream(late.Body).frames(ctx, 1)
	if !strings.Contains(frames[0].data, `"request_id":"while-away"`) || !strings.Contains(frames[0].data, `"replica":"replica-b"`) {
		t.Errorf("snapshot after reconnect lacks B's history: %s", frames[0].data)
	}
	if strings.Contains(frames[0].data, `"hanging"`) {
		t.Errorf("snapshot after reconnect still shows the pre-outage in-flight request: %s", frames[0].data)
	}
}

func TestPeers_NoPeersIsTheOldBehaviour(t *testing.T) {
	rt := newTestRouter(t, nil, WithReplica("solo"))
	srv := eventsServer(t, rt, DashboardConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { rt.RunPeerFeeds(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunPeerFeeds with no peers should return at once")
	}
	if got := rt.PeerStates(); len(got) != 0 {
		t.Errorf("PeerStates = %+v, want none", got)
	}
	resp := openEvents(t, ctx, srv.URL, nil)
	defer resp.Body.Close()
	newSSEStream(resp.Body).frames(ctx, 1)
	rt.Events().Publish(Event{Type: EventFinished, RequestID: "x", Model: "coder", Status: 200})
	var ov struct{ Replica string }
	r, _ := http.Get(srv.URL + "/api/overview")
	_ = json.NewDecoder(r.Body).Decode(&ov)
	r.Body.Close()
	if ov.Replica != "solo" {
		t.Errorf("overview replica = %q, want the WithReplica name", ov.Replica)
	}
	if evs := rt.Events().Replay(); len(evs) != 1 || evs[0].Replica != "solo" {
		t.Errorf("local events should be stamped with the replica name: %+v", evs)
	}
}

func TestPeers_StopStreamsEndsOpenEventStreams(t *testing.T) {
	rt := newTestRouter(t, nil, WithReplica("solo"))
	srv := eventsServer(t, rt, DashboardConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp := openEvents(t, ctx, srv.URL, nil)
	defer resp.Body.Close()
	stream := newSSEStream(resp.Body)
	stream.frames(ctx, 1)
	rt.StopStreams()
	select {
	case _, open := <-stream.lines:
		if open {
			t.Fatal("stream still delivering after StopStreams")
		}
	case <-ctx.Done():
		t.Fatal("stream did not end after StopStreams")
	}
	rt.StopStreams() // idempotent
}

// overviewOf fetches a replica's /api/overview (ungated) with an optional
// query, decoded loosely.
func overviewOf(t *testing.T, url, query string) map[string]any {
	t.Helper()
	resp, err := http.Get(url + "/api/overview" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func replicasOf(t *testing.T, ov map[string]any) []map[string]any {
	t.Helper()
	raw, _ := ov["replicas"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any))
	}
	return out
}

func TestPeers_OverviewDescribesTheFleet(t *testing.T) {
	oldTTL := peerOverviewTTL
	peerOverviewTTL = 200 * time.Millisecond
	t.Cleanup(func() { peerOverviewTTL = oldTTL })
	p := newPeerPair(t, false, WithVersion("v-next"))
	waitPeer(t, p.a, p.srvB.URL, "connected", connected)
	waitPeer(t, p.b, p.srvA.URL, "connected", connected)
	// Anything cached while the feeds were still coming up must lapse.
	time.Sleep(peerOverviewTTL + 50*time.Millisecond)
	for i := 0; i < 3; i++ {
		postChat(t, p.a, `{"model":"no-such-model","messages":[]}`)
	}
	for i := 0; i < 2; i++ {
		postChat(t, p.b, `{"model":"no-such-model","messages":[]}`)
	}

	ov := overviewOf(t, p.srvA.URL, "")
	reps := replicasOf(t, ov)
	if len(reps) != 2 || reps[0]["replica"] != "replica-a" || reps[1]["replica"] != "replica-b" {
		t.Fatalf("replicas = %+v, want self then peer", reps)
	}
	b := reps[1]
	if b["reachable"] != true || b["version"] != "v-next" || b["url"] != p.srvB.URL {
		t.Errorf("peer entry = %+v", b)
	}
	if feed, _ := b["feed"].(map[string]any); feed == nil || feed["connected"] != true {
		t.Errorf("peer feed state missing or down: %+v", b["feed"])
	}
	if reps[0]["feed"] != nil || reps[0]["url"] != nil {
		t.Errorf("self entry should carry no url/feed: %+v", reps[0])
	}
	if got := ov["requests_per_min"].(float64); got < 5 {
		t.Errorf("fleet requests_per_min = %v, want >= 5 (3 on A + 2 on B)", got)
	}
	if ov["version"] != "dev" || ov["replica"] != "replica-a" {
		t.Errorf("top-level identity should stay this replica's: %v %v", ov["version"], ov["replica"])
	}

	// The cache: a second call inside the TTL does not probe B again; one
	// after it does.
	before := p.bProbes.Load()
	overviewOf(t, p.srvA.URL, "")
	if got := p.bProbes.Load(); got != before {
		t.Errorf("probe count after a cached call = %d, want %d", got, before)
	}
	time.Sleep(peerOverviewTTL + 50*time.Millisecond)
	overviewOf(t, p.srvA.URL, "")
	if got := p.bProbes.Load(); got != before+1 {
		t.Errorf("probe count after the TTL = %d, want %d", got, before+1)
	}

	// scope=local (what a peer asks) is this replica alone and its own rate.
	local := overviewOf(t, p.srvA.URL, "?scope=local")
	if lr := replicasOf(t, local); len(lr) != 1 || lr[0]["replica"] != "replica-a" {
		t.Errorf("scope=local replicas = %+v", lr)
	}
	if got := local["requests_per_min"].(float64); got < 3 || got >= 5 {
		t.Errorf("scope=local requests_per_min = %v, want A's own (3)", got)
	}

	// B goes away: still listed, unreachable with a reason, and out of the sum.
	p.srvB.Listener.Close()
	p.srvB.CloseClientConnections()
	p.srvB.Close()
	waitPeer(t, p.a, p.srvB.URL, "disconnected", func(s PeerState) bool { return !s.Connected })
	time.Sleep(peerOverviewTTL + 50*time.Millisecond)
	ov = overviewOf(t, p.srvA.URL, "")
	reps = replicasOf(t, ov)
	if len(reps) != 2 || reps[1]["reachable"] != false || reps[1]["error"] == "" || reps[1]["replica"] != "replica-b" {
		t.Errorf("after B is gone: %+v", reps)
	}
	if got := ov["requests_per_min"].(float64); got >= 5 {
		t.Errorf("fleet rate still counts the unreachable peer: %v", got)
	}
}
