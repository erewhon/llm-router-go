package router

import (
	"net/http"
	"strconv"

	"github.com/erewhon/llm-router-go/internal/auth"
	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// Session listings: every request one harness session made, newest first.
// The session id is the caller's own (Record.SessionID), so this is the
// router side of `pitf session <id>`.
const (
	sessionRowsDefault = 100
	sessionRowsMax     = 500
	// sessionPrefixMin keeps a stray keystroke from listing everyone's
	// traffic: four hex characters of a UUID are already specific.
	sessionPrefixMin = 4
)

func (rt *Router) handleDashRequests(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("session")
	if len(prefix) < sessionPrefixMin {
		writeDashError(w, http.StatusBadRequest, "session must be at least 4 characters of a session id")
		return
	}
	limit := sessionRowsDefault
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			writeDashError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(n, sessionRowsMax)
	}
	reader, ok := rt.sink.(reqlog.SessionReader)
	if !ok {
		writeDashJSON(w, map[string]any{"available": false, "reason": "the request log is not queryable (no reqlog sink)"})
		return
	}
	// Same rule as the traffic tab: a scoped (non-owner) identity sees only
	// its own principal's requests. No identity means no secret is configured
	// (local dev), where every dashboard route is open.
	principal := ""
	if id, ok := auth.FromContext(r.Context()); ok && !isOwner(id) {
		if id.Principal == "" {
			// A scoped identity with no principal cannot own any row; an
			// empty filter here would mean "everyone's", so answer nothing.
			writeDashJSON(w, map[string]any{"available": true, "session": prefix, "limit": limit, "rows": []reqlog.RequestRow{}})
			return
		}
		principal = id.Principal
	}
	rows, err := reader.RequestsBySession(r.Context(), prefix, principal, limit)
	if err != nil {
		rt.logger.WarnContext(r.Context(), "session requests query failed", "err", err)
		writeDashJSON(w, map[string]any{"available": false, "reason": "session query failed; see the router log"})
		return
	}
	if rows == nil {
		rows = []reqlog.RequestRow{}
	}
	writeDashJSON(w, map[string]any{
		"available": true,
		"session":   prefix,
		"limit":     limit,
		"rows":      rows,
	})
}
