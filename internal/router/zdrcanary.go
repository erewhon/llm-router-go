package router

// The ZDR canary: prove the OpenRouter account is still zero-retention.
//
// WHAT IT WATCHES. The fleet's OpenRouter account has account-level zero data
// retention switched on. That setting lives in a web console — not in
// models.yaml, not in this repo, not in anything version control can show a
// diff of. If someone turns it off, every or/* request in the fleet quietly
// stops being zero-retention and NOT ONE FILE CHANGES. There is no error, no
// failed deploy, no log line. That is the exact shape of failure this canary
// exists to catch, and the reason it is worth a request every few hours.
//
// HOW IT WORKS — AND WHY THE PASS CONDITION IS A FAILURE. Account-level ZDR
// makes OpenRouter refuse endpoints that retain, so asking for one is a probe.
// Requesting anthropic/claude-sonnet-5 pinned to provider {"only":["anthropic"]}
// with NO zdr flag was refused on 2026-09-06 with:
//
//	ZDR violation (account settings): 1 endpoint excluded
//
// So: the request FAILING that way is the healthy state. If it starts
// SUCCEEDING, account ZDR is off. A canary whose green light is an error
// response reads backwards, which is why this comment is long and why the
// states are named rather than boolean.
//
// WHAT IT DOES NOT DO. It never gates traffic. The per-request directive in
// privacy.go is the actual enforcement and does not depend on the account
// setting; this only reports. A canary that could take the router down would
// be a bigger outage risk than the posture drift it watches for.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ZDRPosture is what the last canary probe concluded.
type ZDRPosture string

const (
	// ZDRPostureUnknown means no probe has completed yet, or the last one
	// could not reach a conclusion (network error, missing key, 5xx).
	// Deliberately distinct from "not enforced": "we could not tell" and "we
	// checked and it is off" call for different reactions.
	ZDRPostureUnknown ZDRPosture = "unknown"
	// ZDRPostureEnforced means the probe was refused for the expected reason:
	// account-level ZDR is on.
	ZDRPostureEnforced ZDRPosture = "enforced"
	// ZDRPostureNotEnforced means a request that should have been refused was
	// served. Account-level ZDR is OFF.
	ZDRPostureNotEnforced ZDRPosture = "not_enforced"
)

// ZDRStatus is one canary result, cached for /health and the dashboard.
type ZDRStatus struct {
	Posture   ZDRPosture `json:"posture"`
	CheckedAt time.Time  `json:"checked_at,omitempty"`
	// Detail is the upstream's own words where there are any, so a change in
	// OpenRouter's error wording shows up as a readable detail rather than as
	// a silent reclassification to "unknown".
	Detail string `json:"detail,omitempty"`
}

// ZDRCanary probes account posture on an interval.
type ZDRCanary struct {
	client   *http.Client
	logger   *slog.Logger
	apiBase  string
	apiKey   string
	model    string
	interval time.Duration

	mu   sync.RWMutex
	last ZDRStatus
}

// zdrCanaryProvider is the provider the probe pins to. It must be one that
// retains — the point is to ask for something account ZDR should forbid.
// Anthropic's own endpoint retains for 30 days, which makes it the reliable
// choice; the Bedrock and Vertex mirrors of the same model do not, and pinning
// one of those would make the canary pass for the wrong reason.
const zdrCanaryProvider = "anthropic"

// zdrViolationMarker is the substring identifying the healthy refusal. Matched
// loosely (lowercased, on the distinctive half of the phrase) so a reworded
// error still classifies correctly — the alternative, an exact match, would
// degrade to "unknown" on a copy edit and train everyone to ignore it.
const zdrViolationMarker = "zdr violation"

// NewZDRCanary builds a canary. A blank apiKey yields a canary that reports
// ZDRPostureUnknown forever rather than a nil pointer: a router configured
// without an OpenRouter key has nothing to watch, and that is a normal state,
// not an error.
func NewZDRCanary(apiBase, apiKey, model string, interval time.Duration, logger *slog.Logger) *ZDRCanary {
	if logger == nil {
		logger = slog.Default()
	}
	return &ZDRCanary{
		client:   &http.Client{Timeout: 30 * time.Second},
		logger:   logger,
		apiBase:  strings.TrimSuffix(apiBase, "/"),
		apiKey:   apiKey,
		model:    model,
		interval: interval,
		last:     ZDRStatus{Posture: ZDRPostureUnknown},
	}
}

// Status returns the last result.
func (c *ZDRCanary) Status() ZDRStatus {
	if c == nil {
		return ZDRStatus{Posture: ZDRPostureUnknown}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.last
}

// Run probes once immediately, then on the interval, until ctx is done.
// Intended to be started in its own goroutine.
func (c *ZDRCanary) Run(ctx context.Context) {
	if c == nil || c.apiKey == "" {
		return
	}
	c.probe(ctx)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.probe(ctx)
		}
	}
}

// probe runs one check and records the verdict.
func (c *ZDRCanary) probe(ctx context.Context) {
	st := c.check(ctx)
	c.mu.Lock()
	prev := c.last.Posture
	c.last = st
	c.mu.Unlock()

	switch st.Posture {
	case ZDRPostureNotEnforced:
		// ERROR, every time, not once on transition. This is a standing
		// compliance fact, and a single line at the moment of the flip would
		// scroll away long before anyone looked.
		c.logger.Error("OpenRouter account-level ZDR is NOT enforced — or/* traffic is no longer zero-retention by account policy; per-request enforcement still applies to roles that require it",
			"model", c.model, "provider", zdrCanaryProvider, "detail", st.Detail)
	case ZDRPostureEnforced:
		if prev != ZDRPostureEnforced {
			c.logger.Info("OpenRouter account-level ZDR confirmed enforced", "detail", st.Detail)
		}
	case ZDRPostureUnknown:
		c.logger.Warn("ZDR canary inconclusive; account posture unverified", "detail", st.Detail)
	}
}

// check performs the probe and classifies the outcome.
//
// max_tokens is 1 because in the healthy case nothing is generated at all —
// the request is refused before it reaches a model — and in the unhealthy case
// the canary should cost one token, not a paragraph.
func (c *ZDRCanary) check(ctx context.Context) ZDRStatus {
	now := time.Now()
	body, err := json.Marshal(map[string]any{
		"model":      c.model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"provider":   map[string]any{"only": []string{zdrCanaryProvider}},
	})
	if err != nil {
		return ZDRStatus{Posture: ZDRPostureUnknown, CheckedAt: now, Detail: "encode probe: " + err.Error()}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ZDRStatus{Posture: ZDRPostureUnknown, CheckedAt: now, Detail: "build probe: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return ZDRStatus{Posture: ZDRPostureUnknown, CheckedAt: now, Detail: "probe request: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return ZDRStatus{Posture: ZDRPostureUnknown, CheckedAt: now, Detail: "read probe response: " + err.Error()}
	}
	return classifyZDRProbe(resp.StatusCode, raw, now)
}

// classifyZDRProbe turns a probe response into a posture.
//
// Split out from check so the classification — the part with the inverted
// logic that is easy to get backwards — is testable without a live account.
//
// OpenRouter reports this refusal INSIDE a 200 as well as via a 4xx, so the
// status code alone decides nothing: the marker is searched for in the body
// either way. A 2xx with no marker and actual content is the alarm state.
func classifyZDRProbe(status int, raw []byte, at time.Time) ZDRStatus {
	lower := strings.ToLower(string(raw))
	if strings.Contains(lower, zdrViolationMarker) {
		return ZDRStatus{Posture: ZDRPostureEnforced, CheckedAt: at, Detail: extractZDRMessage(raw)}
	}

	// A non-2xx without the marker is some other failure — a bad key, a rate
	// limit, an outage. It proves nothing about retention posture, so it must
	// NOT be read as either verdict.
	if status < 200 || status > 299 {
		return ZDRStatus{
			Posture:   ZDRPostureUnknown,
			CheckedAt: at,
			Detail:    fmt.Sprintf("HTTP %d without a ZDR marker: %s", status, extractZDRMessage(raw)),
		}
	}

	// A clean 2xx means the retaining endpoint served the request. The account
	// is not filtering on data policy.
	return ZDRStatus{
		Posture:   ZDRPostureNotEnforced,
		CheckedAt: at,
		Detail:    fmt.Sprintf("provider %q served the probe; expected an account-policy refusal", zdrCanaryProvider),
	}
}

// extractZDRMessage pulls the upstream's error message out of a response,
// falling back to a truncated body. Best-effort by design: the detail string
// is for a human reading /health, so a rough answer beats an empty one.
func extractZDRMessage(raw []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
