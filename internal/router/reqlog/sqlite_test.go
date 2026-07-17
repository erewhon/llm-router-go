package reqlog

import (
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestSQLiteSink_RoundTrip writes records through the async sink, closes it to
// drain, then reopens the file and asserts the round-trip values — including
// nullable tokens (present vs NULL), the RFC3339Nano ts, and INTEGER booleans
// scanning back into Go bools. Unlike the Postgres test, this needs no external
// DB, so it runs in CI.
func TestSQLiteSink_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reqlog.db")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	sink, err := NewSQLite(path, logger)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}

	pt, ct, tt := 7, 11, 18
	ts := time.Date(2026, 7, 17, 5, 6, 7, 0, time.UTC)
	sink.Log(Record{
		RequestID: "rt-1", TS: ts, Method: "POST", Path: "/v1/chat/completions",
		Model: "research", BackendModel: "nemotron-3-super",
		BackendURL: "http://192.168.42.240:5392", ResolvedVia: "nemotron-3-super",
		APIClass: "chat", ViaToolProxy: true, Stream: true, Status: 200, LatencyMS: 123,
		PromptTokens: &pt, CompletionTokens: &ct, TotalTokens: &tt,
	})
	// A record with nil tokens and empty optional fields → those columns NULL.
	sink.Log(Record{Method: "POST", Path: "/v1/embeddings", Model: "embedding", Status: 200, LatencyMS: 5})

	if err := sink.Close(); err != nil { // Close drains the async writer
		t.Fatalf("Close: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open for verify: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM router_requests").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("row count = %d, want 2", count)
	}

	var (
		model, backendModel, apiClass, tsStr string
		via, stream                          bool
		status, latency                      int
		prompt, completion, total            *int
	)
	err = db.QueryRow(`SELECT model, backend_model, api_class, ts, via_tool_proxy, stream,
	                          status, latency_ms, prompt_tokens, completion_tokens, total_tokens
	                     FROM router_requests WHERE request_id = ?`, "rt-1").
		Scan(&model, &backendModel, &apiClass, &tsStr, &via, &stream,
			&status, &latency, &prompt, &completion, &total)
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if model != "research" || backendModel != "nemotron-3-super" || apiClass != "chat" ||
		!via || !stream || status != 200 || latency != 123 {
		t.Errorf("round-trip mismatch: model=%q backend=%q class=%q via=%v stream=%v status=%d latency=%d",
			model, backendModel, apiClass, via, stream, status, latency)
	}
	if prompt == nil || *prompt != 7 || completion == nil || *completion != 11 || total == nil || *total != 18 {
		t.Errorf("token round-trip wrong: pt=%v ct=%v tt=%v", prompt, completion, total)
	}
	if want := ts.Format(time.RFC3339Nano); tsStr != want {
		t.Errorf("ts = %q, want %q", tsStr, want)
	}

	// The nil-token record must persist NULLs, not zeros.
	var p2 *int
	var backendModel2 *string
	if err := db.QueryRow(`SELECT prompt_tokens, backend_model FROM router_requests WHERE model = ?`, "embedding").
		Scan(&p2, &backendModel2); err != nil {
		t.Fatalf("QueryRow (nil-token): %v", err)
	}
	if p2 != nil {
		t.Errorf("prompt_tokens = %v, want NULL", *p2)
	}
	if backendModel2 != nil {
		t.Errorf("backend_model = %q, want NULL", *backendModel2)
	}
}

// TestSQLiteSink_MigratesOldSchema opens a DB created by an older binary (the
// v0.5.0 schema, before the Anthropic cache/prefix columns) and asserts
// NewSQLite adds the missing columns without losing existing rows, then accepts
// a record that populates them.
func TestSQLiteSink_MigratesOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Pre-v0.6 schema: no cache_creation_input_tokens / cache_read_input_tokens
	// / prefix_hash_chain columns.
	const oldSchema = `
CREATE TABLE router_requests (
    id INTEGER PRIMARY KEY AUTOINCREMENT, request_id TEXT, ts TEXT NOT NULL,
    method TEXT NOT NULL, path TEXT NOT NULL, model TEXT NOT NULL,
    backend_model TEXT, backend_url TEXT, resolved_via TEXT, api_class TEXT,
    via_tool_proxy INTEGER NOT NULL DEFAULT 0, stream INTEGER NOT NULL DEFAULT 0,
    status INTEGER NOT NULL, latency_ms INTEGER NOT NULL,
    prompt_tokens INTEGER, completion_tokens INTEGER, total_tokens INTEGER, error TEXT);`
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	if _, err := seed.Exec(oldSchema); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	if _, err := seed.Exec(
		`INSERT INTO router_requests (ts, method, path, model, status, latency_ms) VALUES (?,?,?,?,?,?)`,
		"2026-07-17T00:00:00Z", "POST", "/v1/chat/completions", "old-model", 200, 10); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	seed.Close()

	// Opening through NewSQLite must migrate the table in place.
	sink, err := NewSQLite(path, logger)
	if err != nil {
		t.Fatalf("NewSQLite (migrate): %v", err)
	}
	cc, cr := 100, 900
	sink.Log(Record{
		Method: "POST", Path: "/v1/messages", Model: "claude-sonnet-4-5", APIClass: "anthropic",
		Status: 200, LatencyMS: 42,
		CacheCreationInputTokens: &cc, CacheReadInputTokens: &cr, PrefixHashChain: "aa,bb,cc",
	})
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM router_requests").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("row count = %d, want 2 (legacy row preserved + new row)", n)
	}
	var (
		cc2, cr2 *int
		chain    *string
	)
	if err := db.QueryRow(`SELECT cache_creation_input_tokens, cache_read_input_tokens, prefix_hash_chain
	                         FROM router_requests WHERE model = ?`, "claude-sonnet-4-5").
		Scan(&cc2, &cr2, &chain); err != nil {
		t.Fatalf("query migrated row: %v", err)
	}
	if cc2 == nil || *cc2 != 100 || cr2 == nil || *cr2 != 900 || chain == nil || *chain != "aa,bb,cc" {
		t.Errorf("migrated columns wrong: cc=%v cr=%v chain=%v", cc2, cr2, chain)
	}
}
