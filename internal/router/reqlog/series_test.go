package reqlog

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// seriesFixture: six records across two models, two principals and two
// 15-minute buckets, with one server failure, one connect failure and one
// scope refusal (which is not a failure).
func seriesFixture(now time.Time) []Record {
	pt, ct := 10, 20
	cost := 0.5
	b0 := now.Add(-40 * time.Minute) // bucket A
	b1 := now.Add(-10 * time.Minute) // bucket B
	return []Record{
		{TS: b0, Model: "coder", ResolvedVia: "alpha", Principal: "steven", Status: 200, LatencyMS: 100, PromptTokens: &pt, CompletionTokens: &ct},
		{TS: b0.Add(time.Minute), Model: "coder", ResolvedVia: "alpha", Principal: "steven", Status: 502, LatencyMS: 300, ErrorClass: "server_error", BackendURL: "http://a"},
		{TS: b1, Model: "beta", ResolvedVia: "beta", Principal: "family", Status: 200, LatencyMS: 50, UpstreamCostUSD: &cost},
		{TS: b1.Add(time.Minute), Model: "beta", ResolvedVia: "beta", Principal: "family", Status: 502, LatencyMS: 10, ErrorClass: "connect", BackendURL: "http://b"},
		{TS: b1.Add(2 * time.Minute), Model: "beta", ResolvedVia: "", Principal: "family", Status: 403, LatencyMS: 1, ErrorClass: "scope_refused"},
		{TS: now.Add(-3 * time.Hour), Model: "old", ResolvedVia: "alpha", Principal: "steven", Status: 200, LatencyMS: 1}, // outside the window
	}
}

func checkSeries(t *testing.T, name string, r SeriesReader, now time.Time) {
	t.Helper()
	ctx := context.Background()
	since := now.Add(-time.Hour)
	rows, err := r.Series(ctx, since, 15*time.Minute, "model", 10)
	if err != nil {
		t.Fatalf("%s Series: %v", name, err)
	}
	if len(rows) != 3 { // alpha, beta, "" (the refused request resolved to nothing)
		t.Fatalf("%s rows = %d (%+v), want 3", name, len(rows), keysOf(rows))
	}
	if rows[0].Key != "alpha" || rows[0].Requests != 2 || rows[1].Key != "beta" || rows[1].Requests != 2 {
		t.Errorf("%s ordering = %v (alpha 2, beta 2: key order on a tie)", name, keysOf(rows))
	}
	byKey := map[string]SeriesRow{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	// Every row carries the full grid.
	n := len(byKey["alpha"].Buckets)
	if n < 4 || n != len(byKey["beta"].Buckets) {
		t.Errorf("%s bucket grid = %d/%d, want the same full hour grid", name, n, len(byKey["beta"].Buckets))
	}
	// alpha: both requests in the bucket ~40 min ago; one error; tokens 10/20.
	var a SeriesBucket
	for _, b := range byKey["alpha"].Buckets {
		if b.Requests > 0 {
			a = b
		}
	}
	if a.Requests != 2 || a.Errors != 1 || a.PromptTokens != 10 || a.CompletionTokens != 20 || a.LatencyAvgMS != 200 {
		t.Errorf("%s alpha bucket = %+v", name, a)
	}
	var b SeriesBucket
	for _, bb := range byKey["beta"].Buckets {
		if bb.Requests > 0 {
			b = bb
		}
	}
	if b.Requests != 2 || b.Errors != 1 || b.CostUSD != 0.5 {
		t.Errorf("%s beta bucket = %+v", name, b)
	}
	// The refused request: a row of its own, no error counted.
	if e := byKey[""]; e.Requests != 1 || sumErrors(e) != 0 {
		t.Errorf("%s unresolved row = %+v, want 1 request, 0 errors (refusals are policy)", name, e)
	}

	// limit folds the tail into "other".
	rows, err = r.Series(ctx, since, 15*time.Minute, "model", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Key != "alpha" || rows[1].Key != "other" || rows[1].Requests != 3 {
		t.Errorf("%s limit=1 rows = %v / other=%d, want [alpha other(3)]", name, keysOf(rows), rows[len(rows)-1].Requests)
	}
	// Group by principal.
	rows, err = r.Series(ctx, since, 15*time.Minute, "principal", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Key != "family" || rows[0].Requests != 3 || rows[1].Key != "steven" {
		t.Errorf("%s by principal = %v", name, keysOf(rows))
	}
	if _, err := r.Series(ctx, since, time.Minute, "nope", 1); err == nil {
		t.Errorf("%s: bad group_by should error", name)
	}

	// Failures: the 502 and the connect, never the refusal.
	fails, err := r.Failures(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(fails) != 2 {
		t.Fatalf("%s failures = %+v, want 2", name, fails)
	}
	for _, f := range fails {
		if f.ErrorClass == "scope_refused" || f.Count != 1 || f.Last.IsZero() {
			t.Errorf("%s failure row = %+v", name, f)
		}
	}
}

func keysOf(rows []SeriesRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Key)
	}
	return out
}
func sumErrors(r SeriesRow) int {
	n := 0
	for _, b := range r.Buckets {
		n += b.Errors
	}
	return n
}

func TestSeries_MemoryAndSQLiteAgree(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	recs := seriesFixture(now)

	mem := &MemorySink{}
	for _, r := range recs {
		mem.Log(r)
	}
	checkSeries(t, "memory", mem, now)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sink, err := NewSQLite(filepath.Join(t.TempDir(), "reqlog.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		sink.Log(r)
	}
	// Drain the async writer without closing the DB: Close drains too, but
	// then the queries have no connection. Give the writer a moment.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := sink.Series(context.Background(), now.Add(-time.Hour), 15*time.Minute, "model", 10)
		if err == nil && len(rows) == 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	checkSeries(t, "sqlite", sink, now)
	_ = sink.Close()
}
