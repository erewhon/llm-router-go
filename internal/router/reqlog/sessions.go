package reqlog

// One harness session's requests, for the dashboard's Requests view and the
// `pitf session` jump. Keyed on session_id, the caller's own session id (see
// Record.SessionID); a prefix is accepted because that is how people paste a
// UUID.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// RequestRow is one request in a session listing. Nullable counts stay nil
// so "no usage reported" is distinct from zero.
type RequestRow struct {
	TS               time.Time `json:"ts"`
	RequestID        string    `json:"request_id"`
	SessionID        string    `json:"session_id"`
	Path             string    `json:"path"`
	Model            string    `json:"model"`
	ResolvedVia      string    `json:"resolved_via"`
	Status           int       `json:"status"`
	LatencyMS        int       `json:"latency_ms"`
	PromptTokens     *int      `json:"prompt_tokens"`
	CompletionTokens *int      `json:"completion_tokens"`
	CacheReadTokens  *int      `json:"cache_read_tokens"`
	UpstreamProvider string    `json:"upstream_provider"`
	ErrorClass       string    `json:"error_class"`
	Principal        string    `json:"principal"`
}

// SessionReader is the optional per-session side of a Sink, asked for by type
// assertion like SeriesReader. Called from a dashboard request, never the
// proxy.
type SessionReader interface {
	// RequestsBySession lists requests whose session_id starts with prefix
	// (case-insensitive), newest first, at most limit. A non-empty principal
	// restricts the rows to that principal's own requests.
	RequestsBySession(ctx context.Context, prefix, principal string, limit int) ([]RequestRow, error)
}

// likePrefix escapes LIKE metacharacters so a pasted id matches literally.
func likePrefix(prefix string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(strings.ToLower(prefix)) + "%"
}

const sessionRequestsSQL = `
SELECT ts, COALESCE(request_id, ''), COALESCE(session_id, ''), path, model,
       COALESCE(resolved_via, ''), status, latency_ms,
       prompt_tokens, completion_tokens, cache_read_input_tokens,
       COALESCE(upstream_provider, ''), COALESCE(error_class, ''), COALESCE(principal, '')
FROM router_requests
WHERE session_id IS NOT NULL AND lower(session_id) LIKE ? ESCAPE '\'%s
ORDER BY ts DESC
LIMIT ?`

func sessionQuery(prefix, principal string, limit int) (string, []any) {
	args := []any{likePrefix(prefix)}
	extra := ""
	if principal != "" {
		extra = " AND principal = ?"
		args = append(args, principal)
	}
	args = append(args, limit)
	return fmt.Sprintf(sessionRequestsSQL, extra), args
}

// RequestsBySession implements SessionReader over Postgres.
func (s *PostgresSink) RequestsBySession(ctx context.Context, prefix, principal string, limit int) ([]RequestRow, error) {
	q, args := sessionQuery(prefix, principal, limit)
	rows, err := s.pool.Query(ctx, rebindPG(q), args...)
	if err != nil {
		return nil, fmt.Errorf("reqlog: session query: %w", err)
	}
	defer rows.Close()
	var out []RequestRow
	for rows.Next() {
		var r RequestRow
		if err := rows.Scan(&r.TS, &r.RequestID, &r.SessionID, &r.Path, &r.Model,
			&r.ResolvedVia, &r.Status, &r.LatencyMS,
			&r.PromptTokens, &r.CompletionTokens, &r.CacheReadTokens,
			&r.UpstreamProvider, &r.ErrorClass, &r.Principal); err != nil {
			return nil, fmt.Errorf("reqlog: session scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RequestsBySession implements SessionReader over SQLite.
func (s *SQLiteSink) RequestsBySession(ctx context.Context, prefix, principal string, limit int) ([]RequestRow, error) {
	q, args := sessionQuery(prefix, principal, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("reqlog: session query: %w", err)
	}
	defer rows.Close()
	var out []RequestRow
	for rows.Next() {
		var r RequestRow
		var ts string
		if err := rows.Scan(&ts, &r.RequestID, &r.SessionID, &r.Path, &r.Model,
			&r.ResolvedVia, &r.Status, &r.LatencyMS,
			&r.PromptTokens, &r.CompletionTokens, &r.CacheReadTokens,
			&r.UpstreamProvider, &r.ErrorClass, &r.Principal); err != nil {
			return nil, fmt.Errorf("reqlog: session scan: %w", err)
		}
		r.TS, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RequestsBySession implements SessionReader in memory, for router tests.
func (m *MemorySink) RequestsBySession(_ context.Context, prefix, principal string, limit int) ([]RequestRow, error) {
	want := strings.ToLower(prefix)
	var out []RequestRow
	for _, rec := range m.Records() {
		if rec.SessionID == "" || !strings.HasPrefix(strings.ToLower(rec.SessionID), want) {
			continue
		}
		if principal != "" && rec.Principal != principal {
			continue
		}
		out = append(out, RequestRow{
			TS: rec.TS, RequestID: rec.RequestID, SessionID: rec.SessionID, Path: rec.Path,
			Model: rec.Model, ResolvedVia: rec.ResolvedVia, Status: rec.Status, LatencyMS: rec.LatencyMS,
			PromptTokens: rec.PromptTokens, CompletionTokens: rec.CompletionTokens,
			CacheReadTokens: rec.CacheReadInputTokens, UpstreamProvider: rec.UpstreamProvider,
			ErrorClass: rec.ErrorClass, Principal: rec.Principal,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.After(out[j].TS) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
