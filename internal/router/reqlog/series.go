package reqlog

// Time series over the request log, for the dashboard's Traffic tab: what
// happened over the last hour, day or week, from the durable record rather
// than the in-memory windows that reset on every restart. Sits beside
// UsageByPrincipal and follows its rules: a "failure" is an error class other
// than client_error, or a 5xx on a pre-instrumentation row; refusals
// (privacy_refused / scope_refused) are policy, not failures, and are
// excluded the same way.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SeriesBucket is one time slot of one series row.
type SeriesBucket struct {
	Start            time.Time `json:"start"`
	Requests         int       `json:"requests"`
	Errors           int       `json:"errors"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	// LatencyAvgMS is the mean latency in the bucket. A true p50 would need
	// a per-bucket ordered scan on SQLite; the mean is what both backends can
	// give cheaply, and the chart labels it as such.
	LatencyAvgMS int `json:"latency_avg_ms"`
}

// SeriesRow is one series: a model (resolved_via) or a principal, with its
// buckets in time order. Key "other" folds everything beyond the limit;
// "" is the unattributed principal / an unresolved request.
type SeriesRow struct {
	Key      string         `json:"key"`
	Requests int            `json:"requests"`
	Buckets  []SeriesBucket `json:"buckets"`
}

// FailureRow is one (model, backend, error class) that failed in the window.
type FailureRow struct {
	Model      string    `json:"model"`
	BackendURL string    `json:"backend_url"`
	ErrorClass string    `json:"error_class"`
	Count      int       `json:"count"`
	Last       time.Time `json:"last"`
}

// SeriesReader is the optional time-series side of a Sink, asked for by type
// assertion like UsageReader. Called from a dashboard poll, never the proxy.
type SeriesReader interface {
	// Series buckets requests since `since` by `bucket` width, grouped by
	// "model" (resolved_via) or "principal". Rows are ordered by total
	// requests desc; at most limit rows, the rest folded into "other".
	Series(ctx context.Context, since time.Time, bucket time.Duration, groupBy string, limit int) ([]SeriesRow, error)
	// Failures lists what failed in the window by (model, backend, class).
	Failures(ctx context.Context, since time.Time) ([]FailureRow, error)
}

// failureCond is the shared definition, with refusals excluded.
const failureCond = `((error_class IS NOT NULL AND error_class NOT IN ('client_error', 'privacy_refused', 'scope_refused'))
       OR (error_class IS NULL AND status >= 500))`

// seriesSQL has two placeholders filled by the backend: the bucket-start
// expression and the group column. The "?" is `since`.
const seriesSQL = `
SELECT %s AS key, %s AS bucket_start,
       COUNT(*) AS requests,
       SUM(CASE WHEN ` + failureCond + ` THEN 1 ELSE 0 END) AS errors,
       COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
       COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
       COALESCE(SUM(upstream_cost_usd), 0) AS cost_usd,
       COALESCE(AVG(latency_ms), 0) AS latency_avg_ms
FROM router_requests
WHERE ts > ?
GROUP BY 1, 2
ORDER BY 2 ASC`

const failuresSQL = `
SELECT COALESCE(resolved_via, '') AS model, COALESCE(backend_url, '') AS backend_url,
       COALESCE(error_class, '') AS error_class, COUNT(*) AS count, MAX(ts) AS last
FROM router_requests
WHERE ts > ? AND ` + failureCond + `
GROUP BY 1, 2, 3
ORDER BY count DESC, model ASC`

func groupColumn(groupBy string) (string, error) {
	switch groupBy {
	case "model":
		return "COALESCE(resolved_via, '')", nil
	case "principal":
		return "COALESCE(principal, '')", nil
	}
	return "", fmt.Errorf("reqlog: series group_by %q (want model or principal)", groupBy)
}

// seriesPoint is one (key, bucket) aggregate as scanned from either backend.
type seriesPoint struct {
	key string
	b   SeriesBucket
}

// assembleSeries turns scanned points into ordered rows with the limit/fold
// applied and every row carrying the full bucket grid (zero-filled), so a
// chart can stack them without aligning timestamps itself.
func assembleSeries(points []seriesPoint, since time.Time, bucket time.Duration, limit int, now time.Time) []SeriesRow {
	if bucket <= 0 {
		bucket = time.Hour
	}
	start := since.Truncate(bucket)
	nBuckets := int(now.Sub(start)/bucket) + 1
	if nBuckets < 1 {
		nBuckets = 1
	}
	rows := map[string]*SeriesRow{}
	get := func(key string) *SeriesRow {
		r := rows[key]
		if r == nil {
			r = &SeriesRow{Key: key, Buckets: make([]SeriesBucket, nBuckets)}
			for i := range r.Buckets {
				r.Buckets[i].Start = start.Add(time.Duration(i) * bucket)
			}
			rows[key] = r
		}
		return r
	}
	add := func(dst *SeriesBucket, src SeriesBucket) {
		// Weighted mean across merged points.
		if dst.Requests+src.Requests > 0 {
			dst.LatencyAvgMS = (dst.LatencyAvgMS*dst.Requests + src.LatencyAvgMS*src.Requests) / (dst.Requests + src.Requests)
		}
		dst.Requests += src.Requests
		dst.Errors += src.Errors
		dst.PromptTokens += src.PromptTokens
		dst.CompletionTokens += src.CompletionTokens
		dst.CostUSD += src.CostUSD
	}
	for _, p := range points {
		i := int(p.b.Start.Sub(start) / bucket)
		if i < 0 || i >= nBuckets {
			continue
		}
		r := get(p.key)
		add(&r.Buckets[i], p.b)
		r.Requests += p.b.Requests
	}
	out := make([]SeriesRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Key < out[j].Key
	})
	if limit > 0 && len(out) > limit {
		other := SeriesRow{Key: "other", Buckets: make([]SeriesBucket, nBuckets)}
		for i := range other.Buckets {
			other.Buckets[i].Start = start.Add(time.Duration(i) * bucket)
		}
		for _, r := range out[limit:] {
			other.Requests += r.Requests
			for i := range r.Buckets {
				add(&other.Buckets[i], r.Buckets[i])
			}
		}
		out = append(out[:limit], other)
	}
	return out
}

// Series implements SeriesReader over Postgres.
func (s *PostgresSink) Series(ctx context.Context, since time.Time, bucket time.Duration, groupBy string, limit int) ([]SeriesRow, error) {
	col, err := groupColumn(groupBy)
	if err != nil {
		return nil, err
	}
	secs := int64(bucket / time.Second)
	if secs <= 0 {
		secs = 3600
	}
	bucketExpr := fmt.Sprintf("to_timestamp(floor(extract(epoch from ts) / %d) * %d)", secs, secs)
	q := rebindPG(fmt.Sprintf(seriesSQL, col, bucketExpr))
	rows, err := s.pool.Query(ctx, q, since)
	if err != nil {
		return nil, fmt.Errorf("reqlog: series query: %w", err)
	}
	defer rows.Close()
	var pts []seriesPoint
	for rows.Next() {
		var p seriesPoint
		var lat float64
		if err := rows.Scan(&p.key, &p.b.Start, &p.b.Requests, &p.b.Errors, &p.b.PromptTokens, &p.b.CompletionTokens, &p.b.CostUSD, &lat); err != nil {
			return nil, fmt.Errorf("reqlog: series scan: %w", err)
		}
		p.b.LatencyAvgMS = int(lat)
		pts = append(pts, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return assembleSeries(pts, since, bucket, limit, time.Now()), nil
}

// Failures implements SeriesReader over Postgres.
func (s *PostgresSink) Failures(ctx context.Context, since time.Time) ([]FailureRow, error) {
	rows, err := s.pool.Query(ctx, rebindPG(failuresSQL), since)
	if err != nil {
		return nil, fmt.Errorf("reqlog: failures query: %w", err)
	}
	defer rows.Close()
	var out []FailureRow
	for rows.Next() {
		var r FailureRow
		var last *time.Time
		if err := rows.Scan(&r.Model, &r.BackendURL, &r.ErrorClass, &r.Count, &last); err != nil {
			return nil, fmt.Errorf("reqlog: failures scan: %w", err)
		}
		if last != nil {
			r.Last = *last
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Series implements SeriesReader over SQLite. ts is RFC3339 text there, so
// the bucket is (unix seconds / width) * width via strftime.
func (s *SQLiteSink) Series(ctx context.Context, since time.Time, bucket time.Duration, groupBy string, limit int) ([]SeriesRow, error) {
	col, err := groupColumn(groupBy)
	if err != nil {
		return nil, err
	}
	secs := int64(bucket / time.Second)
	if secs <= 0 {
		secs = 3600
	}
	bucketExpr := fmt.Sprintf("(CAST(strftime('%%s', ts) AS INTEGER) / %d) * %d", secs, secs)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(seriesSQL, col, bucketExpr), since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("reqlog: series query: %w", err)
	}
	defer rows.Close()
	var pts []seriesPoint
	for rows.Next() {
		var p seriesPoint
		var epoch int64
		var cost sql.NullFloat64
		var lat sql.NullFloat64
		if err := rows.Scan(&p.key, &epoch, &p.b.Requests, &p.b.Errors, &p.b.PromptTokens, &p.b.CompletionTokens, &cost, &lat); err != nil {
			return nil, fmt.Errorf("reqlog: series scan: %w", err)
		}
		p.b.Start = time.Unix(epoch, 0).UTC()
		p.b.CostUSD = cost.Float64
		p.b.LatencyAvgMS = int(lat.Float64)
		pts = append(pts, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return assembleSeries(pts, since, bucket, limit, time.Now()), nil
}

// Failures implements SeriesReader over SQLite.
func (s *SQLiteSink) Failures(ctx context.Context, since time.Time) ([]FailureRow, error) {
	rows, err := s.db.QueryContext(ctx, failuresSQL, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("reqlog: failures query: %w", err)
	}
	defer rows.Close()
	var out []FailureRow
	for rows.Next() {
		var r FailureRow
		var last sql.NullString
		if err := rows.Scan(&r.Model, &r.BackendURL, &r.ErrorClass, &r.Count, &last); err != nil {
			return nil, fmt.Errorf("reqlog: failures scan: %w", err)
		}
		if last.Valid {
			r.Last = parseSQLiteTS(last.String)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func parseSQLiteTS(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func isFailure(rec Record) bool {
	if rec.ErrorClass != "" {
		return rec.ErrorClass != "client_error" && !strings.HasSuffix(rec.ErrorClass, "_refused")
	}
	return rec.Status >= 500
}

// Series implements SeriesReader over the in-memory buffer, for tests.
func (m *MemorySink) Series(_ context.Context, since time.Time, bucket time.Duration, groupBy string, limit int) ([]SeriesRow, error) {
	if _, err := groupColumn(groupBy); err != nil {
		return nil, err
	}
	if bucket <= 0 {
		bucket = time.Hour
	}
	type acc struct {
		b SeriesBucket
		n int
		l int
	}
	byKB := map[string]map[time.Time]*acc{}
	for _, rec := range m.Records() {
		if !rec.TS.After(since) {
			continue
		}
		key := rec.ResolvedVia
		if groupBy == "principal" {
			key = rec.Principal
		}
		bs := rec.TS.Truncate(bucket)
		if byKB[key] == nil {
			byKB[key] = map[time.Time]*acc{}
		}
		a := byKB[key][bs]
		if a == nil {
			a = &acc{b: SeriesBucket{Start: bs}}
			byKB[key][bs] = a
		}
		a.b.Requests++
		if isFailure(rec) {
			a.b.Errors++
		}
		if rec.PromptTokens != nil {
			a.b.PromptTokens += int64(*rec.PromptTokens)
		}
		if rec.CompletionTokens != nil {
			a.b.CompletionTokens += int64(*rec.CompletionTokens)
		}
		if rec.UpstreamCostUSD != nil {
			a.b.CostUSD += *rec.UpstreamCostUSD
		}
		a.l += rec.LatencyMS
		a.n++
	}
	var pts []seriesPoint
	for key, bm := range byKB {
		for _, a := range bm {
			if a.n > 0 {
				a.b.LatencyAvgMS = a.l / a.n
			}
			pts = append(pts, seriesPoint{key: key, b: a.b})
		}
	}
	return assembleSeries(pts, since, bucket, limit, time.Now()), nil
}

// Failures implements SeriesReader over the in-memory buffer.
func (m *MemorySink) Failures(_ context.Context, since time.Time) ([]FailureRow, error) {
	type k struct{ model, base, class string }
	byK := map[k]*FailureRow{}
	for _, rec := range m.Records() {
		if !rec.TS.After(since) || !isFailure(rec) {
			continue
		}
		kk := k{rec.ResolvedVia, rec.BackendURL, rec.ErrorClass}
		r := byK[kk]
		if r == nil {
			r = &FailureRow{Model: rec.ResolvedVia, BackendURL: rec.BackendURL, ErrorClass: rec.ErrorClass}
			byK[kk] = r
		}
		r.Count++
		if rec.TS.After(r.Last) {
			r.Last = rec.TS
		}
	}
	out := make([]FailureRow, 0, len(byK))
	for _, r := range byK {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}
