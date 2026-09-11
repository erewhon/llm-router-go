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
	logger *slog.Logger
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
