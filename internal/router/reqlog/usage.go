package reqlog

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// UsageRow is one principal's traffic over a window: the answer to "whose
// spend is this?" that per-user attribution exists to make askable.
//
// Failures follow the reqlog-failures recipe's definition: an error_class
// other than client_error, or — for rows written by pre-instrumentation
// binaries with no class at all — a 5xx status. Client errors are the
// caller's own doing and are not counted against them here either.
type UsageRow struct {
	// Principal is the attributed caller; "" collects unattributed rows
	// (auth-exempt paths, or a router with auth off) so they are visible
	// rather than silently absent from the total.
	Principal        string `json:"principal"`
	Requests         int    `json:"requests"`
	Failures         int    `json:"failures"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	// CostUSD sums the provider-billed cost of the principal's cloud
	// requests. Local requests cost nothing here by definition.
	CostUSD  float64   `json:"cost_usd"`
	LastSeen time.Time `json:"last_seen"`
}

// UsageReader is the optional read side of a Sink: the dashboard asks for it
// by type assertion, and a sink that cannot answer (NopSink) simply is not
// one. Implementations must not block the request path — this is called from
// a dashboard poll, never from the proxy.
type UsageReader interface {
	UsageByPrincipal(ctx context.Context, since time.Time) ([]UsageRow, error)
}

// usageSQL is the shared aggregate, written once with "?" placeholders and
// rebound for Postgres. Works on both backends: COALESCE, FILTER-free CASE
// sums, and no window functions.
const usageSQL = `
SELECT COALESCE(principal, '') AS principal,
       COUNT(*) AS requests,
       SUM(CASE WHEN (error_class IS NOT NULL AND error_class <> 'client_error')
                  OR (error_class IS NULL AND status >= 500) THEN 1 ELSE 0 END) AS failures,
       COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
       COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
       COALESCE(SUM(upstream_cost_usd), 0) AS cost_usd,
       MAX(ts) AS last_seen
FROM router_requests
WHERE ts > ?
GROUP BY 1
ORDER BY requests DESC, principal ASC`

// UsageByPrincipal implements UsageReader over the shared Postgres table.
func (s *PostgresSink) UsageByPrincipal(ctx context.Context, since time.Time) ([]UsageRow, error) {
	rows, err := s.pool.Query(ctx, rebindPG(usageSQL), since)
	if err != nil {
		return nil, fmt.Errorf("reqlog: usage query: %w", err)
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		var last *time.Time
		if err := rows.Scan(&r.Principal, &r.Requests, &r.Failures, &r.PromptTokens, &r.CompletionTokens, &r.CostUSD, &last); err != nil {
			return nil, fmt.Errorf("reqlog: usage scan: %w", err)
		}
		if last != nil {
			r.LastSeen = *last
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageByPrincipal implements UsageReader over the local SQLite file. The
// pool is one connection shared with the writer goroutine, so this serialises
// behind in-flight inserts — fine for a dashboard poll, which is the only
// caller.
func (s *SQLiteSink) UsageByPrincipal(ctx context.Context, since time.Time) ([]UsageRow, error) {
	rows, err := s.db.QueryContext(ctx, usageSQL, since)
	if err != nil {
		return nil, fmt.Errorf("reqlog: usage query: %w", err)
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		var last sql.NullString
		var cost sql.NullFloat64
		if err := rows.Scan(&r.Principal, &r.Requests, &r.Failures, &r.PromptTokens, &r.CompletionTokens, &cost, &last); err != nil {
			return nil, fmt.Errorf("reqlog: usage scan: %w", err)
		}
		r.CostUSD = cost.Float64
		if last.Valid {
			// The driver hands back whatever text it stored for the
			// timestamp; parse the layouts it uses, and leave the zero time
			// rather than fail the whole panel over one unparseable cell.
			for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999"} {
				if t, err := time.Parse(layout, last.String); err == nil {
					r.LastSeen = t
					break
				}
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageByPrincipal implements UsageReader over the in-memory buffer, so the
// dashboard panel is testable without a database.
func (m *MemorySink) UsageByPrincipal(_ context.Context, since time.Time) ([]UsageRow, error) {
	byP := map[string]*UsageRow{}
	for _, rec := range m.Records() {
		if !rec.TS.After(since) {
			continue
		}
		r := byP[rec.Principal]
		if r == nil {
			r = &UsageRow{Principal: rec.Principal}
			byP[rec.Principal] = r
		}
		r.Requests++
		if (rec.ErrorClass != "" && rec.ErrorClass != "client_error") || (rec.ErrorClass == "" && rec.Status >= 500) {
			r.Failures++
		}
		if rec.PromptTokens != nil {
			r.PromptTokens += int64(*rec.PromptTokens)
		}
		if rec.CompletionTokens != nil {
			r.CompletionTokens += int64(*rec.CompletionTokens)
		}
		if rec.UpstreamCostUSD != nil {
			r.CostUSD += *rec.UpstreamCostUSD
		}
		if rec.TS.After(r.LastSeen) {
			r.LastSeen = rec.TS
		}
	}
	out := make([]UsageRow, 0, len(byP))
	for _, r := range byP {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Principal < out[j].Principal
	})
	return out, nil
}

// rebindPG turns "?" placeholders into "$1..$N". The usage query has exactly
// one, but numbering rather than string-replacing keeps this correct if it
// ever grows a second.
func rebindPG(q string) string {
	out := make([]byte, 0, len(q)+8)
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			out = append(out, '$')
			out = append(out, []byte(fmt.Sprint(n))...)
			continue
		}
		out = append(out, q[i])
	}
	return string(out)
}
