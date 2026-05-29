package reqlog

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"
)

// TestPostgresSink_RoundTrip exercises the real driver against a real
// Postgres. Skipped unless ROUTER_REQLOG_PG_DSN is set in the environment
// (e.g. `ROUTER_REQLOG_PG_DSN=postgres://postgres:test@127.0.0.1:5439/postgres
// go test ./internal/router/reqlog/...`).
//
// The test inserts one record via the async writer, polls until the row
// appears, then asserts the round-trip values match. We keep the sink alive
// for the whole test via a defer Close (which is idempotent), and use the
// sink's own pool for the verifying SELECT — no second pool needed.
func TestPostgresSink_RoundTrip(t *testing.T) {
	dsn := os.Getenv("ROUTER_REQLOG_PG_DSN")
	if dsn == "" {
		t.Skip("ROUTER_REQLOG_PG_DSN not set; skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sink, err := NewPostgres(ctx, dsn, logger)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	defer sink.Close()

	// Clean slate (a previous run may have left this row).
	if _, err := sink.pool.Exec(ctx, "DELETE FROM router_requests WHERE request_id = $1", "test-rt-1"); err != nil {
		t.Fatalf("cleanup delete: %v", err)
	}

	pt, ct, tt := 7, 11, 18
	sink.Log(Record{
		RequestID:        "test-rt-1",
		Method:           "POST",
		Path:             "/v1/chat/completions",
		Model:            "research",
		BackendModel:     "nemotron-3-super",
		BackendURL:       "http://192.168.42.240:5392",
		ResolvedVia:      "nemotron-3-super",
		APIClass:         "chat",
		ViaToolProxy:     true,
		Status:           200,
		LatencyMS:        123,
		PromptTokens:     &pt,
		CompletionTokens: &ct,
		TotalTokens:      &tt,
	})

	// Async writer — poll until the row appears (or give up after ~2s).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := sink.pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM router_requests WHERE request_id = $1", "test-rt-1").
			Scan(&n); err == nil && n == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	var (
		model, backendModel, apiClass string
		status, latency               int
		via                           bool
		prompt, completion, total     *int
	)
	err = sink.pool.QueryRow(ctx,
		`SELECT model, backend_model, api_class, via_tool_proxy, status, latency_ms,
		        prompt_tokens, completion_tokens, total_tokens
		   FROM router_requests
		  WHERE request_id = $1`, "test-rt-1").
		Scan(&model, &backendModel, &apiClass, &via, &status, &latency,
			&prompt, &completion, &total)
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if model != "research" || backendModel != "nemotron-3-super" ||
		apiClass != "chat" || !via || status != 200 || latency != 123 {
		t.Errorf("round-trip mismatch: model=%q backend_model=%q api_class=%q via=%v status=%d latency=%d",
			model, backendModel, apiClass, via, status, latency)
	}
	if prompt == nil || *prompt != 7 || completion == nil || *completion != 11 || total == nil || *total != 18 {
		t.Errorf("token round-trip wrong: pt=%v ct=%v tt=%v", prompt, completion, total)
	}

	// Close is idempotent: an explicit Close before the defer must not panic.
	if err := sink.Close(); err != nil {
		t.Errorf("explicit Close: %v", err)
	}
}
