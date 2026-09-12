package router

// GET /api/traffic — the Traffic tab's durable series from the request log,
// with fixed windows so the chart code stays simple: 1h in 1-minute buckets,
// 24h in 15-minute buckets, 7d in 1-hour buckets. Behind the identity gate
// like /api/usage (principals are in the data); a non-owner asking by
// principal sees only their own row, and by model the full view (no
// principal in it).

import (
	"net/http"
	"time"

	"github.com/erewhon/llm-router-go/internal/auth"
	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// trafficWindows maps the window query value to (span, bucket).
var trafficWindows = map[string][2]time.Duration{
	"1h":  {time.Hour, time.Minute},
	"24h": {24 * time.Hour, 15 * time.Minute},
	"7d":  {7 * 24 * time.Hour, time.Hour},
}

const trafficRowLimit = 12

func (rt *Router) handleDashTraffic(w http.ResponseWriter, r *http.Request) {
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "24h"
	}
	by := r.URL.Query().Get("by")
	if by == "" {
		by = "model"
	}
	span, ok := trafficWindows[window]
	if !ok {
		writeDashError(w, http.StatusBadRequest, "window must be one of 1h, 24h, 7d")
		return
	}
	if by != "model" && by != "principal" {
		writeDashError(w, http.StatusBadRequest, "by must be model or principal")
		return
	}
	reader, ok := rt.sink.(reqlog.SeriesReader)
	if !ok {
		writeDashJSON(w, map[string]any{"available": false, "reason": "the request log is not queryable (no reqlog sink)"})
		return
	}
	since := time.Now().Add(-span[0])
	rows, err := reader.Series(r.Context(), since, span[1], by, trafficRowLimit)
	if err != nil {
		rt.logger.WarnContext(r.Context(), "traffic series query failed", "err", err)
		writeDashJSON(w, map[string]any{"available": false, "reason": "series query failed; see the router log"})
		return
	}
	failures, err := reader.Failures(r.Context(), since)
	if err != nil {
		rt.logger.WarnContext(r.Context(), "traffic failures query failed", "err", err)
		failures = nil
	}
	if id, ok := auth.FromContext(r.Context()); ok && !isOwner(id) && by == "principal" {
		own := rows[:0]
		for _, row := range rows {
			if row.Key == id.Principal {
				own = append(own, row)
			}
		}
		rows = own
	}
	if rows == nil {
		rows = []reqlog.SeriesRow{}
	}
	if failures == nil {
		failures = []reqlog.FailureRow{}
	}
	writeDashJSON(w, map[string]any{
		"available": true,
		"window":    window,
		"bucket_s":  int(span[1] / time.Second),
		"by":        by,
		"since":     since,
		"rows":      rows,
		"failures":  failures,
	})
}
