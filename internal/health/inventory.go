package health

// Live inventory: what each upstream says it serves, right now.
//
// /v1/models and the OpenCode well-known used to be rendered from the
// registry alone, so they advertised what models.yaml *said* — not what could
// serve. Two failure shapes hid behind that. Drift: a seat whose port was
// reassigned to a different model (archimedes:5391 was registered as
// qwen3.5-122b-a10b while serving Qwen3-Coder-Next-FP8, 2026-09-12), or a
// provider that retired an id, stayed advertised and stayed a role candidate.
// And the opposite: when OpenCode Zen turned a new model on, nobody saw it
// until someone edited YAML. PAIR builds its inventory by fanning out to every
// node and merging the replies; this is the same idea against every distinct
// upstream base the registry names.
//
// The poll is part of the tracker's cycle but on its own interval (60 s by
// default, not the 15 s node poll — a listing changes on the timescale of a
// deploy, not a heartbeat). Every base is fetched concurrently with a hard
// per-fetch timeout, results are applied as each lands, and nothing on the
// request path ever waits for one: a slow provider costs its own base a late
// refresh and nobody else anything.
//
// Two verdicts come out of a listing. For a hand-written entry whose served
// name is missing from its base's list — and the list fetched OK — the entry
// is `absent`: not routable, dropped from /v1/models and the well-known,
// shown on the dashboard with the reason. A base whose fetch failed keeps its
// last-known list; stale is better than empty, and the age is visible. For a
// base named in the `discovery:` block, listed ids that the adopt policy
// accepts and that no hand-written entry, alias or role already claims become
// virtual external entries under the source's prefix — routable by name and
// listed, never joined to a role or chain. A discovered id that disappears
// from its provider is retired after two consecutive misses.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
)

const (
	// DefaultInventoryInterval is how often each base's listing is refreshed.
	DefaultInventoryInterval = 60 * time.Second
	// DefaultInventoryTimeout bounds one listing fetch. OpenRouter's
	// catalogue is ~1 MB and answers in well under a second; anything that
	// takes longer than this is treated as unreachable for this round and
	// keeps its last-known list.
	DefaultInventoryTimeout = 5 * time.Second
	// DiscoveryRetireAfter is how many consecutive successful fetches must
	// omit a discovered id before it is dropped. Two, so a single flapping
	// listing never yanks a model out from under a session.
	DiscoveryRetireAfter = 2
)

// Listing is one base's live model list, keyed by the id the base serves.
type Listing map[string]config.ListedModel

// ListFunc fetches one base's listing. root is the api_base with any
// trailing "/v1" removed; "/v1/models" is appended. Injectable so tests stay
// hermetic.
type ListFunc func(ctx context.Context, root, bearer, bearerHeader string) (Listing, error)

// listingClient has no timeout of its own: the per-fetch context carries it.
var listingClient = &http.Client{}

// maxListingBytes bounds a listing body. OpenRouter's is ~1 MB today.
const maxListingBytes = 16 << 20

// FetchListing is the real ListFunc: GET <root>/v1/models, decoded leniently
// so the plain OpenAI shape (id only) and OpenRouter's annotated shape
// (context_length, pricing, architecture, supported_parameters) both work. A
// root written with a trailing "/v1" is tolerated, as it is everywhere else.
func FetchListing(ctx context.Context, root, bearer, bearerHeader string) (Listing, error) {
	url := rootOf(root) + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if bearer != "" {
		if bearerHeader != "" {
			req.Header.Set(bearerHeader, bearer)
		} else {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
	}
	resp, err := listingClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("GET %s: %s: %s", url, resp.Status, oneLine(snippet))
	}
	var doc listingDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxListingBytes)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("GET %s: decode: %w", url, err)
	}
	return doc.listing(), nil
}

// listingDoc is the lenient wire shape. Every field beyond id is optional.
type listingDoc struct {
	Data []struct {
		ID            string `json:"id"`
		ContextLength int    `json:"context_length"`
		Pricing       struct {
			Prompt     json.RawMessage `json:"prompt"`
			Completion json.RawMessage `json:"completion"`
		} `json:"pricing"`
		TopProvider struct {
			MaxCompletionTokens *int `json:"max_completion_tokens"`
		} `json:"top_provider"`
		Architecture struct {
			InputModalities []string `json:"input_modalities"`
		} `json:"architecture"`
		SupportedParameters []string `json:"supported_parameters"`
	} `json:"data"`
}

func (d listingDoc) listing() Listing {
	out := make(Listing, len(d.Data))
	for _, m := range d.Data {
		if m.ID == "" {
			continue
		}
		lm := config.ListedModel{ID: m.ID, ContextLength: m.ContextLength}
		if m.TopProvider.MaxCompletionTokens != nil && *m.TopProvider.MaxCompletionTokens > 0 {
			lm.MaxOutputTokens = *m.TopProvider.MaxCompletionTokens
		}
		lm.InputCostPerMillion = perMillion(m.Pricing.Prompt)
		lm.OutputCostPerMillion = perMillion(m.Pricing.Completion)
		for _, mod := range m.Architecture.InputModalities {
			if mod == "image" {
				lm.Vision = true
			}
		}
		for _, p := range m.SupportedParameters {
			if p == "tools" || p == "tool_choice" {
				lm.ToolCalling = true
			}
		}
		out[m.ID] = lm
	}
	return out
}

// perMillion converts a per-token price — OpenRouter sends it as a decimal
// string, some providers as a number — to per-million. Missing, unparsable
// or negative (OpenRouter's "-1" for dynamically priced routers) means
// unknown, which callers treat as "assume it costs money".
func perMillion(raw json.RawMessage) *float64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	s := strings.Trim(string(raw), `"`)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	// Round to a micro-dollar per million: enough for every published rate,
	// and it keeps 3e-06*1e6 from rendering as 2.9999999999999996.
	out := math.Round(v*1e6*1e6) / 1e6
	return &out
}

// InventoryEnabled reports whether the live-inventory poll checks this
// placement's own listing for its served name. Default: on for chat,
// embeddings and rerank entries — the classes whose engines and providers
// list what they serve on /v1/models. Off for media classes (an sd-cpp or
// Orpheus port has no such endpoint), the Anthropic passthrough (its listing
// needs the caller's credential and version header), virtual chains (no base
// of their own) and disabled entries. `health.inventory: true|false` on the
// entry overrides the class default either way.
func InventoryEnabled(m config.ModelDefinition) bool {
	if m.IsVirtual() || !m.Enabled {
		return false
	}
	if m.Health != nil && m.Health.Inventory != nil {
		return *m.Health.Inventory
	}
	switch m.APIClass {
	case config.APIClassChat, config.APIClassEmbeddings, config.APIClassRerank, "":
		return true
	}
	return false
}

// baseInventory is the tracker's bookkeeping for one distinct upstream root.
type baseInventory struct {
	root string
	// bearer/header carry the credential the fetch presents: the discovery
	// source's when the base is one, else the first member's that has one.
	bearer, header string
	// members are the registry ids served at this root, sorted.
	members []string
	// served is every provider-side id some registry entry — enabled or
	// not, in this mode or not — already routes to at this root. Discovery
	// never adopts one of these: the operator has it, under whatever name
	// they chose (or/claude-opus-5 for anthropic/claude-opus-5), or turned
	// it off on purpose.
	served map[string]bool
	// sources are the indexes into Registry.Discovery whose api_base is this
	// root.
	sources []int

	// listing is the last-known GOOD list (nil until the first success);
	// fetchedAt is when it was fetched. err is the most recent failure and is
	// cleared on success, so err != "" with listing != nil is "stale".
	listing     Listing
	fetchedAt   time.Time
	attemptedAt time.Time
	err         string
	inflight    bool
}

// discoveredEntry is one adopted id and its lifecycle.
type discoveredEntry struct {
	def       config.ModelDefinition
	root      string
	source    int
	misses    int
	firstSeen time.Time
}

// BaseInventory is the public view of one base, for /v1/availability,
// /health and the dashboard.
type BaseInventory struct {
	Base string `json:"base"`
	// Members are the hand-written registry ids routed to this base.
	Members []string `json:"members,omitempty"`
	// IDs is the size of the last-known listing; zero until the first
	// successful fetch.
	IDs int `json:"ids"`
	// Listed is the last-known listing itself, sorted. Omitted from /health
	// (an OpenRouter listing is several hundred ids); present on
	// /v1/availability.
	Listed []string `json:"listed,omitempty"`
	// FetchedAt is when the last-known listing was fetched; nil when never.
	FetchedAt *time.Time `json:"fetched_at,omitempty"`
	// AgeS is how old the last-known listing is, in seconds, for a reader
	// that does not want to do date arithmetic. Absent when never fetched.
	AgeS *float64 `json:"age_s,omitempty"`
	// Error is the most recent fetch failure; empty after a success.
	Error string `json:"error,omitempty"`
	// Stale marks a base whose last fetch failed but which still has a
	// last-known listing the verdicts are computed from.
	Stale bool `json:"stale,omitempty"`
	// Absent lists the members whose served name is missing from the listing.
	Absent []string `json:"absent,omitempty"`
	// Discovered lists the ids adopted from this base, by registry id.
	Discovered []string `json:"discovered,omitempty"`
}

// buildInventory derives the base table from the registry once, at
// construction: every distinct root a live-listing check applies to, who is
// served there, and which discovery sources point at it. The table is static
// for the process lifetime, like the registry itself.
func (t *Tracker) buildInventory() {
	t.inv = map[string]*baseInventory{}
	t.discovered = map[string]*discoveredEntry{}
	t.reserved = map[string]bool{}
	reg := t.cfg.Registry
	if reg == nil || t.cfg.DisableInventory {
		return
	}
	for _, name := range reg.ReservedNames() {
		t.reserved[name] = true
	}
	base := func(root string) *baseInventory {
		b := t.inv[root]
		if b == nil {
			b = &baseInventory{root: root, served: map[string]bool{}}
			t.inv[root] = b
		}
		return b
	}

	ids := make([]string, 0, len(reg.Models))
	for id := range reg.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m := reg.Models[id]
		if !InventoryEnabled(m) {
			continue
		}
		if t.cfg.ProbeFilter != nil && !t.cfg.ProbeFilter(id) {
			continue // out of this router's mode: its port is expected dead
		}
		// The engine's own base, tool proxy bypassed — the listing we want is
		// the engine's, not the hop in front of it. Same derivation as the
		// generation probe's target.
		direct := false
		apiBase, err := reg.APIBase(id, &direct)
		if err != nil {
			continue
		}
		root := rootOf(apiBase)
		b := base(root)
		b.members = append(b.members, id)
		if b.bearer == "" && m.Backend == config.BackendExternal && m.APIKey != "" {
			b.bearer = resolveKey(m.APIKey, t.getenv)
			b.header = m.APIKeyHeader
		}
	}
	for i, src := range reg.Discovery {
		b := base(src.Root())
		b.sources = append(b.sources, i)
		if src.APIKey != "" {
			// A source's own credential outranks a member's: it is the one
			// the operator wrote next to the adopt policy.
			b.bearer = resolveKey(src.APIKey, t.getenv)
			b.header = src.APIKeyHeader
		}
	}
	// What each base already serves for the registry, over EVERY entry —
	// disabled rollback entries and out-of-mode seats included. Only bases
	// that exist matter (a disabled entry alone does not create one).
	for _, id := range ids {
		m := reg.Models[id]
		if m.IsVirtual() {
			continue
		}
		direct := false
		apiBase, err := reg.APIBase(id, &direct)
		if err != nil {
			continue
		}
		if b := t.inv[rootOf(apiBase)]; b != nil {
			b.served[m.BackendModelName()] = true
		}
	}
}

// rootOf normalises an api_base to the root a request path is appended to.
func rootOf(apiBase string) string {
	return strings.TrimSuffix(strings.TrimSuffix(apiBase, "/"), "/v1")
}

// invJob is one due fetch, captured under the lock.
type invJob struct {
	root, bearer, header string
}

// dueInventoryLocked marks and returns the bases whose refresh is due: not
// already in flight, and past the interval since their last attempt. Caller
// holds t.mu.
func (t *Tracker) dueInventoryLocked() []invJob {
	if t.cfg.DisableInventory || t.list == nil {
		return nil
	}
	now := t.now()
	var jobs []invJob
	for root, b := range t.inv {
		if b.inflight {
			continue
		}
		if !b.attemptedAt.IsZero() && now.Sub(b.attemptedAt) < t.cfg.InventoryInterval {
			continue
		}
		b.inflight = true
		b.attemptedAt = now
		jobs = append(jobs, invJob{root: root, bearer: b.bearer, header: b.header})
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].root < jobs[j].root })
	return jobs
}

// startInventory launches the due fetches, each in its own goroutine under
// its own timeout, applying each result as it lands. It returns at once: the
// poll loop, the OnPoll callback and every request see a base's new listing
// the moment that base answers, and never wait for the slowest one.
func (t *Tracker) startInventory(ctx context.Context, jobs []invJob) {
	for _, job := range jobs {
		t.invWG.Add(1)
		go func(job invJob) {
			defer t.invWG.Done()
			fctx, cancel := context.WithTimeout(ctx, t.cfg.InventoryTimeout)
			defer cancel()
			l, err := t.list(fctx, job.root, job.bearer, job.header)
			t.mu.Lock()
			t.applyInventoryLocked(job.root, l, err)
			t.mu.Unlock()
		}(job)
	}
}

// WaitInventory blocks until every in-flight listing fetch has been applied.
// For tests and the synchronous RefreshInventory; production never waits.
func (t *Tracker) WaitInventory() { t.invWG.Wait() }

// RefreshInventory fetches every base now, regardless of interval, and waits
// for the results. An operator-triggered refresh; also how tests step the
// inventory deterministically.
func (t *Tracker) RefreshInventory(ctx context.Context) {
	t.mu.Lock()
	for _, b := range t.inv {
		if !b.inflight {
			b.attemptedAt = time.Time{}
		}
	}
	jobs := t.dueInventoryLocked()
	t.mu.Unlock()
	t.startInventory(ctx, jobs)
	t.WaitInventory()
}

// applyInventoryLocked folds one fetch outcome into the base and everything
// that hangs off it. Caller holds t.mu.
func (t *Tracker) applyInventoryLocked(root string, l Listing, err error) {
	b := t.inv[root]
	if b == nil {
		return
	}
	b.inflight = false
	now := t.now()
	if err != nil {
		if b.err == "" {
			// Log on the transition only; a provider that is down for an
			// hour would otherwise say so sixty times.
			if b.listing != nil {
				t.logger.Warn("inventory fetch failed; keeping last-known listing",
					"base", root, "err", err.Error(), "listing_age", now.Sub(b.fetchedAt).Round(time.Second).String())
			} else {
				t.logger.Warn("inventory fetch failed; no listing yet", "base", root, "err", err.Error())
			}
		}
		b.err = err.Error()
		return
	}
	if b.err != "" {
		t.logger.Info("inventory fetch recovered", "base", root, "ids", len(l))
	} else if b.listing == nil {
		t.logger.Info("inventory listed", "base", root, "ids", len(l), "members", len(b.members))
	}
	b.err = ""
	b.listing = l
	b.fetchedAt = now

	reg := t.cfg.Registry
	for _, id := range b.members {
		m := reg.Models[id]
		st := t.stateFor(id)
		_, listed := l[m.BackendModelName()]
		switch {
		case !listed && !st.absent:
			st.absent = true
			st.absentReason = absentReason(root, m.BackendModelName(), l)
			t.logger.Warn("model absent from its backend's listing", "model", id, "reason", st.absentReason)
		case listed && st.absent:
			st.absent = false
			st.absentReason = ""
			t.logger.Info("model listed again", "model", id, "base", root)
		}
	}

	for _, si := range b.sources {
		src := reg.Discovery[si]
		for id, lm := range l {
			if !src.Adopts(id) {
				continue
			}
			if b.served[id] {
				continue // some models.yaml entry already routes to this id here
			}
			full := src.Prefix + id
			if t.reserved[full] {
				continue // a hand-written entry, alias or role owns this name
			}
			if e := t.discovered[full]; e != nil {
				e.def = src.Entry(lm) // pricing and limits follow the provider
				e.misses = 0
				continue
			}
			t.discovered[full] = &discoveredEntry{def: src.Entry(lm), root: root, source: si, firstSeen: now}
			t.logger.Info("model discovered", "model", full, "base", root,
				"context", lm.ContextLength, "priced", lm.InputCostPerMillion != nil)
		}
		for full, e := range t.discovered {
			if e.source != si {
				continue
			}
			pid := strings.TrimPrefix(full, src.Prefix)
			if _, ok := l[pid]; ok && src.Adopts(pid) && !b.served[pid] {
				continue
			}
			e.misses++
			if e.misses >= DiscoveryRetireAfter {
				delete(t.discovered, full)
				t.logger.Info("discovered model retired", "model", full, "base", root,
					"misses", e.misses, "lifetime", now.Sub(e.firstSeen).Round(time.Second).String())
			}
		}
	}
}

// absentReason names what the base serves instead, when that is short enough
// to be useful — for a reassigned local port that is the whole diagnosis.
func absentReason(root, want string, l Listing) string {
	reason := fmt.Sprintf("%q is not in the live listing at %s", want, root)
	if len(l) == 0 {
		return reason + " (it lists nothing)"
	}
	if len(l) <= 3 {
		ids := make([]string, 0, len(l))
		for id := range l {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return reason + " (it serves: " + strings.Join(ids, ", ") + ")"
	}
	return reason + fmt.Sprintf(" (%d ids listed)", len(l))
}

// Discovered returns the current adopted entries by registry id. The map is
// a copy; the router reads it on every lookup that misses the static set,
// so it must be cheap and must never alias tracker state.
func (t *Tracker) Discovered() map[string]config.ModelDefinition {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.discovered) == 0 {
		return nil
	}
	out := make(map[string]config.ModelDefinition, len(t.discovered))
	for id, e := range t.discovered {
		out[id] = e.def
	}
	return out
}

// Inventory returns the public view of every base, sorted by root.
// withListed includes each base's full id list; /health passes false.
func (t *Tracker) Inventory(withListed bool) []BaseInventory {
	t.mu.RLock()
	defer t.mu.RUnlock()
	roots := make([]string, 0, len(t.inv))
	for r := range t.inv {
		roots = append(roots, r)
	}
	sort.Strings(roots)
	now := t.now()
	out := make([]BaseInventory, 0, len(roots))
	for _, r := range roots {
		b := t.inv[r]
		v := BaseInventory{Base: r, Members: append([]string(nil), b.members...), IDs: len(b.listing), Error: b.err}
		if b.listing != nil {
			at := b.fetchedAt
			v.FetchedAt = &at
			age := now.Sub(at).Seconds()
			v.AgeS = &age
			v.Stale = b.err != ""
			if withListed {
				v.Listed = make([]string, 0, len(b.listing))
				for id := range b.listing {
					v.Listed = append(v.Listed, id)
				}
				sort.Strings(v.Listed)
			}
		}
		for _, id := range b.members {
			if st := t.models[id]; st != nil && st.absent {
				v.Absent = append(v.Absent, id)
			}
		}
		for id, e := range t.discovered {
			if e.root == r {
				v.Discovered = append(v.Discovered, id)
			}
		}
		sort.Strings(v.Discovered)
		out = append(out, v)
	}
	return out
}
