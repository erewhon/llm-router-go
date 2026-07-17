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
