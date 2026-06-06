package router

import (
	"encoding/json"
	"net/http"
	"strings"
)

// RequireBearer returns a middleware that gates requests behind an
// `Authorization: Bearer <key>` header. Paths in exempt bypass auth
// entirely (typically /health, /metrics, /.well-known/opencode — they're
// either probes that must always work, or the discovery endpoint that
// clients use to learn the key in the first place).
//
// If keys is empty (or contains only blanks), the middleware is a
// no-op. This keeps local development ergonomic; the operator opts in
// to auth by passing --api-keys.
func RequireBearer(keys []string, exempt []string) func(http.Handler) http.Handler {
	keySet := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			keySet[k] = struct{}{}
		}
	}
	exemptSet := make(map[string]struct{}, len(exempt))
	for _, p := range exempt {
		exemptSet[p] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		if len(keySet) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := exemptSet[r.URL.Path]; ok {
				next.ServeHTTP(w, r)
				return
			}
			const prefix = "Bearer "
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, prefix) {
				writeAuthError(w, "missing bearer token")
				return
			}
			if _, ok := keySet[auth[len(prefix):]]; !ok {
				writeAuthError(w, "invalid api key")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
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
