package router

// Dashboard identity and token self-service.
//
// The dashboard listener has no auth of its own and trusts nothing about who
// is calling. Behind the hub that is fine for the read-only panels: Caddy's
// forward_auth to oauth2-proxy gates the vhost, and Caddy copies the resolved
// identity through as X-Auth-Request-*. It is NOT fine for minting
// credentials, because nothing on this listener proves those headers came
// from Caddy: anything that can reach the VIP directly can set them and claim
// to be anyone, which would turn header forgery into token issuance as an
// arbitrary principal — the exact opposite of per-user attribution.
//
// The gate is a shared secret: Caddy adds X-Dashboard-Auth on every proxied
// request, and an identity header is believed only when that secret matches.
// Cheapest of the options weighed in the Forge task (strip inbound headers /
// shared secret / verify the OIDC token / bind off the VIP) that defeats both
// header forgery AND direct-to-VIP reach in one move, with no live dependency
// on the IdP. The same gate covers /api/chat, which spends money and was
// protected by nothing but network position, and /api/usage, which names
// people.
//
// Without a secret configured, the listener has no way to know who is
// calling: token routes are refused outright, and chat + usage stay open
// exactly as before — the local-development posture, on loopback.

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/erewhon/llm-router-go/internal/auth"
	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// DashboardAuthHeader carries the shared secret from the front proxy.
const DashboardAuthHeader = "X-Dashboard-Auth"

// dashboardTokenID is the reqlog token_id stamped on requests a person makes
// from the dashboard itself (quick chat). It is not a real token id — there
// is no credential to revoke — but it keeps those rows attributed to the
// person rather than falling into the unattributed bucket.
const dashboardTokenID = "dashboard"

// familyScope is the widest scope a non-owner may mint, and the scope their
// own dashboard chat runs under. A family token must be structurally unable
// to reach a paid provider; the dashboard's chat box is the same person at
// the same trust boundary, so it gets the same fence.
const familyScope = auth.ScopeModelsLocal

// dashIdentityGate builds the middleware that turns a proxy-asserted identity
// into an auth.Identity on the request context, or refuses.
func (rt *Router) dashIdentityGate(cfg DashboardConfig) func(http.Handler) http.Handler {
	hdr := cfg.IdentityHeader
	if hdr == "" {
		hdr = "X-Auth-Request-Email"
	}
	owners := make(map[string]struct{}, len(cfg.Owners))
	for _, o := range cfg.Owners {
		if o = normalizePrincipal(o); o != "" {
			owners[o] = struct{}{}
		}
	}
	secret := []byte(cfg.AuthSecret)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			isTokens := strings.HasPrefix(r.URL.Path, "/api/tokens")
			if len(secret) == 0 {
				if isTokens {
					writeDashError(w, http.StatusServiceUnavailable,
						"token self-service is not configured on this listener (no --dashboard-auth-secret), so it cannot know who you are")
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.Header.Get(DashboardAuthHeader)), secret) != 1 {
				// Deliberately the same answer whether the header is absent
				// or wrong, and no hint about which one.
				writeDashError(w, http.StatusUnauthorized,
					"this route needs the front proxy's identity; reach the dashboard through its public URL")
				return
			}
			principal := normalizePrincipal(r.Header.Get(hdr))
			switch {
			case principal == "":
				writeDashError(w, http.StatusUnauthorized, "the front proxy sent no identity ("+hdr+")")
				return
			case strings.HasPrefix(principal, auth.LegacyPrincipalPrefix):
				// The legacy namespace is synthetic and must stay that way.
				writeDashError(w, http.StatusForbidden, "that principal is reserved")
				return
			}
			if isTokens && cfg.Tokens == nil {
				writeDashError(w, http.StatusServiceUnavailable, "no token store is configured on this router (--pat-dsn / --pat-db)")
				return
			}
			id := auth.Identity{Principal: principal, TokenID: dashboardTokenID}
			if _, owner := owners[principal]; !owner {
				id.Scopes = []string{familyScope}
			}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	}
}

// normalizePrincipal folds case and whitespace so "Steven@Example.org " and
// "steven@example.org" are one person in the store and in reqlog.
func normalizePrincipal(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// isOwner reports whether the identity on the context may mint any scope.
// Owners carry no scope from the gate; everyone else carries familyScope.
func isOwner(id auth.Identity) bool { return len(id.Scopes) == 0 }

// allowedScopes lists what this identity may request at mint time, widest
// first so a UI can default to the safest by reading the last element.
func allowedScopes(id auth.Identity) []string {
	if isOwner(id) {
		return auth.KnownScopes()
	}
	return []string{familyScope}
}

// dashToken is the wire shape of one token in a list: everything but the
// secret hash, which never leaves the store package.
type dashToken struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Scope     string     `json:"scope"`
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at"`
	State     string     `json:"state"`
}

func toDashToken(t auth.Token, now time.Time) dashToken {
	state := "active"
	if err := t.Active(now); err != nil {
		switch {
		case errors.Is(err, auth.ErrRevoked):
			state = "revoked"
		case errors.Is(err, auth.ErrExpired):
			state = "expired"
		}
	}
	return dashToken{
		ID:        t.ID,
		Label:     t.Label,
		Scope:     auth.Identity{Scopes: t.Scopes}.ModelScope(),
		CreatedAt: t.CreatedAt,
		LastUsed:  t.LastUsedAt,
		ExpiresAt: t.ExpiresAt,
		RevokedAt: t.RevokedAt,
		State:     state,
	}
}

// handleDashTokensList — GET /api/tokens: the caller's own tokens, and what
// the caller may mint. Never another principal's.
func (rt *Router) handleDashTokensList(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	toks, err := rt.dashConfig.Tokens.List(id.Principal)
	if err != nil {
		rt.writeStoreError(w, err)
		return
	}
	now := time.Now()
	out := make([]dashToken, 0, len(toks))
	for _, t := range toks {
		out = append(out, toDashToken(t, now))
	}
	writeDashJSON(w, map[string]any{
		"configured":     true,
		"principal":      id.Principal,
		"owner":          isOwner(id),
		"allowed_scopes": allowedScopes(id),
		"tokens":         out,
	})
}

// handleDashTokensMint — POST /api/tokens {label, scope, expires_days}.
//
// The principal comes from the verified identity and from nowhere else. A
// "principal" field in the body is ignored, not honoured: honouring it would
// make "mint as someone else" a matter of typing their name.
func (rt *Router) handleDashTokensMint(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	var req struct {
		Label       string `json:"label"`
		Scope       string `json:"scope"`
		ExpiresDays int    `json:"expires_days"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeDashError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	req.Label = strings.TrimSpace(req.Label)
	if req.Label == "" {
		writeDashError(w, http.StatusBadRequest, `"label" is required — say what the token is for (laptop, opencode, night-agent)`)
		return
	}
	if len(req.Label) > 80 {
		writeDashError(w, http.StatusBadRequest, `"label" is too long (max 80 characters)`)
		return
	}
	// Default to the SAFEST scope the caller may have, not the widest: a
	// family member who leaves the field blank gets local-only, and an owner
	// who wants unrestricted must say so.
	scope := strings.TrimSpace(req.Scope)
	if scope == "" {
		scope = familyScope
	}
	if err := auth.ValidateScopes([]string{scope}); err != nil {
		writeDashError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !isOwner(id) && !auth.ScopeAtMost(scope, familyScope) {
		writeDashError(w, http.StatusForbidden,
			"your account may mint "+familyScope+" tokens only; widening a scope is an owner action")
		return
	}
	var expires *time.Time
	switch {
	case req.ExpiresDays < 0 || req.ExpiresDays > 3660:
		writeDashError(w, http.StatusBadRequest, `"expires_days" must be 0 (never) or between 1 and 3660`)
		return
	case req.ExpiresDays > 0:
		t := time.Now().UTC().Add(time.Duration(req.ExpiresDays) * 24 * time.Hour)
		expires = &t
	}

	wire, tok, err := rt.dashConfig.Tokens.Mint(id.Principal, req.Label, []string{scope}, expires)
	if err != nil {
		rt.writeStoreError(w, err)
		return
	}
	rt.logger.InfoContext(r.Context(), "PAT minted from dashboard",
		"principal", tok.Principal, "token_id", tok.ID, "scope", scope, "label", tok.Label)
	// The secret exists in recoverable form in this one response and never
	// again. No caching, no re-fetch, no log line.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":         tok.ID,
		"principal":  tok.Principal,
		"label":      tok.Label,
		"scope":      scope,
		"expires_at": tok.ExpiresAt,
		"token":      wire,
		"note":       "Copy it now — only its hash is stored, so it cannot be shown again.",
	})
}

// handleDashTokensRevoke — DELETE /api/tokens/{id}: revoke, only if the token
// belongs to the caller. Another principal's token and a nonexistent id get
// the same 404.
func (rt *Router) handleDashTokensRevoke(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	tokID := r.PathValue("id")
	if tokID == "" {
		writeDashError(w, http.StatusBadRequest, "token id required")
		return
	}
	err := rt.dashConfig.Tokens.RevokeOwned(tokID, id.Principal)
	switch {
	case errors.Is(err, auth.ErrUnknown):
		writeDashError(w, http.StatusNotFound, "no such token")
		return
	case err != nil:
		rt.writeStoreError(w, err)
		return
	}
	rt.logger.InfoContext(r.Context(), "PAT revoked from dashboard", "principal", id.Principal, "token_id", tokID)
	w.WriteHeader(http.StatusNoContent)
}

// handleDashUsage — GET /api/usage?hours=24: requests, tokens and
// provider-billed cost per principal over the window, from the reqlog sink.
// A sink that cannot answer (NopSink) yields available:false rather than an
// error, so the panel hides itself instead of alarming.
//
// Non-owners see their own row only. Usage names people; the family fleet's
// per-person spend is the owner's to read, not each other's.
func (rt *Router) handleDashUsage(w http.ResponseWriter, r *http.Request) {
	reader, ok := rt.sink.(reqlog.UsageReader)
	if !ok {
		writeDashJSON(w, map[string]any{"available": false, "reason": "the request log is not queryable (no reqlog sink)"})
		return
	}
	hours := 24
	if h, err := strconv.Atoi(r.URL.Query().Get("hours")); err == nil && h > 0 && h <= 24*90 {
		hours = h
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	rows, err := reader.UsageByPrincipal(r.Context(), since)
	if err != nil {
		rt.logger.WarnContext(r.Context(), "usage query failed", "err", err)
		writeDashJSON(w, map[string]any{"available": false, "reason": "usage query failed; see the router log"})
		return
	}
	if id, ok := auth.FromContext(r.Context()); ok && !isOwner(id) {
		own := rows[:0]
		for _, row := range rows {
			if row.Principal == id.Principal {
				own = append(own, row)
			}
		}
		rows = own
	}
	if rows == nil {
		rows = []reqlog.UsageRow{}
	}
	writeDashJSON(w, map[string]any{"available": true, "hours": hours, "rows": rows})
}

// writeStoreError maps a token-store failure to a response. An unreachable
// store is a 503 that says so — the same rule as the front door: an outage
// must never read as "you did something wrong".
func (rt *Router) writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, auth.ErrStoreUnavailable) {
		w.Header().Set("Retry-After", "5")
		writeDashError(w, http.StatusServiceUnavailable, "the token store is unreachable right now; retry shortly")
		return
	}
	rt.logger.Error("token store operation failed", "err", err)
	writeDashError(w, http.StatusInternalServerError, "token store error: "+err.Error())
}

func writeDashError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})
}
