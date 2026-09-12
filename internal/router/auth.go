package router

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/erewhon/llm-router-go/internal/auth"
)

// Authenticator gates /v1/* behind a bearer credential and resolves it to an
// identity, so a request can be attributed to a person rather than merely
// admitted.
//
// Two credential kinds are accepted concurrently, and that overlap is the
// whole migration plan:
//
//   - Personal access tokens ("pat_..."), looked up in the token store.
//   - Legacy shared keys from --api-keys / $ROUTER_API_KEYS, which resolve to
//     a synthetic "legacy:<fingerprint>" principal.
//
// Legacy keys stay first-class until their traffic reaches zero in reqlog.
// Attributing them (rather than leaving them anonymous) is what makes that
// moment observable instead of a guess.
type Authenticator struct {
	store  *auth.Store
	keys   map[string]struct{}
	exempt map[string]struct{}
	// passthrough paths carry the caller's OWN upstream credential (the
	// Anthropic Messages API) and no router credential. They are never
	// gated — but they are attributed: a PAT sent alongside resolves to a
	// person and is stripped before forwarding; otherwise the upstream
	// credential's fingerprint becomes a synthetic principal. See
	// attributePassthrough.
	passthrough map[string]struct{}
	// keyPrincipals maps an upstream-credential fingerprint to a person, so
	// "anthropic:1a2b3c4d" can read "erewhon@flatland.org" once the operator
	// has seen which key is whose.
	keyPrincipals map[string]string
	logger        *slog.Logger
}

// NewAuthenticator builds the middleware. store may be nil (no PATs
// configured); keys may be empty (no legacy shared keys).
func NewAuthenticator(store *auth.Store, keys, exempt []string, logger *slog.Logger) *Authenticator {
	if logger == nil {
		logger = slog.Default()
	}
	a := &Authenticator{
		store:  store,
		keys:   make(map[string]struct{}, len(keys)),
		exempt: make(map[string]struct{}, len(exempt)),
		logger: logger,
	}
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			a.keys[k] = struct{}{}
		}
	}
	for _, p := range exempt {
		a.exempt[p] = struct{}{}
	}
	return a
}

// Passthrough marks paths whose requests are attributed but never refused
// for lack of a router credential. Returns a for chaining.
func (a *Authenticator) Passthrough(paths ...string) *Authenticator {
	if a.passthrough == nil {
		a.passthrough = make(map[string]struct{}, len(paths))
	}
	for _, p := range paths {
		a.passthrough[p] = struct{}{}
	}
	return a
}

// KeyPrincipals installs the fingerprint → principal table for passthrough
// attribution. Returns a for chaining.
func (a *Authenticator) KeyPrincipals(m map[string]string) *Authenticator {
	a.keyPrincipals = m
	return a
}

// Enabled reports whether any credential source is configured. When nothing
// is, the middleware is a no-op — local development stays frictionless and
// the operator opts in, exactly as the pre-PAT behaviour did.
func (a *Authenticator) Enabled() bool { return a.store != nil || len(a.keys) > 0 }

// Middleware returns the http middleware.
func (a *Authenticator) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if !a.Enabled() {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := a.exempt[r.URL.Path]; ok {
				next.ServeHTTP(w, r)
				return
			}
			if _, ok := a.passthrough[r.URL.Path]; ok {
				a.attributePassthrough(w, r, next)
				return
			}
			const prefix = "Bearer "
			hdr := r.Header.Get("Authorization")
			if !strings.HasPrefix(hdr, prefix) {
				writeAuthError(w, "missing bearer token")
				return
			}
			bearer := hdr[len(prefix):]

			id, err := a.resolve(bearer)
			if errors.Is(err, auth.ErrStoreUnavailable) {
				// The one failure that must NOT look like a credential
				// problem. With a shared Postgres store, this is what a
				// database outage looks like — and answering 401 would send
				// every user in the fleet off rotating tokens that were never
				// broken, mid-incident. 503 says "not you, and try again",
				// which is both true and actionable.
				//
				// ERROR level, not Info: unlike a rejected key, this is the
				// operator's problem and nobody else can fix it.
				a.logger.Error("token store unavailable — answering 503; auth cannot be verified",
					"path", r.URL.Path, "err", err.Error(), "token_id", safeTokenID(bearer))
				writeStoreUnavailable(w)
				return
			}
			if err != nil {
				// One opaque message to the caller regardless of cause; the
				// specific reason goes to the log, where the operator can see
				// it and the caller cannot use it as an oracle.
				a.logger.Info("auth rejected",
					"path", r.URL.Path, "reason", err.Error(), "token_id", safeTokenID(bearer))
				writeAuthError(w, "invalid api key")
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	}
}

// attributePassthrough handles a path that forwards the caller's own upstream
// credential. Nothing here can refuse a request for lacking a ROUTER
// credential — the upstream will judge the credential it is given — but the
// request must not stay anonymous either, since these paths are the bulk of
// front-door traffic (13,468 of ~13,500 requests in the 24h before this was
// written carried no principal).
//
// Precedence:
//
//  1. "Authorization: Bearer pat_…" — a router PAT sent ALONGSIDE the
//     Anthropic x-api-key. Resolved to a real person and then STRIPPED, so
//     the upstream never sees a credential it would reject. A bad PAT is a
//     401: presenting one is opting in to being checked.
//  2. x-api-key — a long-lived Anthropic key. Fingerprinted to
//     "anthropic:<fp>", or to the operator's mapping for that fingerprint.
//  3. "Authorization: Bearer <anything else>" — a Claude Code OAuth token.
//     Fingerprinted to "anthropic-oauth:<fp>". Rotates on refresh, so this
//     is the least stable of the three.
//
// A request with none of these carries no identity and goes upstream to be
// refused there.
func (a *Authenticator) attributePassthrough(w http.ResponseWriter, r *http.Request, next http.Handler) {
	const prefix = "Bearer "
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), prefix)
	if bearer != r.Header.Get("Authorization") && auth.LooksLikePAT(bearer) {
		id, err := a.resolve(bearer)
		switch {
		case errors.Is(err, auth.ErrStoreUnavailable):
			a.logger.Error("token store unavailable — answering 503; auth cannot be verified",
				"path", r.URL.Path, "err", err.Error(), "token_id", safeTokenID(bearer))
			writeStoreUnavailable(w)
			return
		case err != nil:
			a.logger.Info("auth rejected", "path", r.URL.Path, "reason", err.Error(), "token_id", safeTokenID(bearer))
			writeAuthError(w, "invalid api key")
			return
		}
		// The PAT is the router's business only. The upstream gets the
		// caller's x-api-key and nothing that looks like ours.
		r.Header.Del("Authorization")
		next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		return
	}
	var id auth.Identity
	switch {
	case r.Header.Get("x-api-key") != "":
		key := r.Header.Get("x-api-key")
		id = auth.AnthropicKeyIdentity(key, a.keyPrincipals[auth.Fingerprint(key)])
	case bearer != "" && bearer != r.Header.Get("Authorization"):
		id = auth.AnthropicOAuthIdentity(bearer, a.keyPrincipals[auth.Fingerprint(bearer)])
	default:
		next.ServeHTTP(w, r)
		return
	}
	next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
}

// resolve turns a bearer value into an identity. PAT-shaped credentials are
// only ever checked against the store, and shared keys only against the key
// set: a mistyped PAT must not fall through to "invalid api key" via the
// legacy path, or the operator debugging it gets a misleading answer.
func (a *Authenticator) resolve(bearer string) (auth.Identity, error) {
	if auth.LooksLikePAT(bearer) {
		if a.store == nil {
			return auth.Identity{}, errors.New("pat presented but no token store configured")
		}
		return a.store.Verify(bearer)
	}
	if _, ok := a.keys[bearer]; ok {
		return auth.LegacyIdentity(bearer), nil
	}
	return auth.Identity{}, auth.ErrUnknown
}

// safeTokenID extracts the public id half of a PAT for logging. It returns ""
// for anything else — a shared key has no loggable half, and logging a
// fingerprint of a rejected credential would be a slow way to leak a
// dictionary of near-misses.
func safeTokenID(bearer string) string {
	if !auth.LooksLikePAT(bearer) {
		return ""
	}
	id, _, err := auth.Split(bearer)
	if err != nil {
		return ""
	}
	return id
}

// RequireBearer is the pre-PAT constructor: shared keys only, no identity
// store. Retained so callers and tests that predate personal access tokens
// keep working unchanged.
func RequireBearer(keys []string, exempt []string) func(http.Handler) http.Handler {
	return NewAuthenticator(nil, keys, exempt, slog.New(slog.DiscardHandler)).Middleware()
}

// writeStoreUnavailable answers a request whose credential could not be
// checked because the token store is unreachable.
//
// 503 with Retry-After, deliberately distinct from the 401 every credential
// failure gets. The message names the store rather than the caller's key: the
// single most expensive mistake this code could make is telling a hundred
// clients their credentials are bad when the database is simply down.
func writeStoreUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "5")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type":    "service_unavailable",
			"code":    "token_store_unavailable",
			"message": "cannot verify credentials right now: the token store is unreachable. Your API key is probably fine; retry shortly.",
		},
	})
}

func writeAuthError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type":    "authentication_error",
			"message": msg,
		},
	})
}
