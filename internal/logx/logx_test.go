package logx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
		err  bool
	}{
		{"", slog.LevelInfo, false},
		{"info", slog.LevelInfo, false},
		{"INFO", slog.LevelInfo, false},
		{"debug", slog.LevelDebug, false},
		{"warn", slog.LevelWarn, false},
		{"warning", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"err", slog.LevelError, false},
		{"trace", 0, true},
	}
	for _, c := range cases {
		got, err := ParseLevel(c.in)
		if (err != nil) != c.err {
			t.Errorf("ParseLevel(%q) err=%v want err=%v", c.in, err, c.err)
		}
		if !c.err && got != c.want {
			t.Errorf("ParseLevel(%q) = %v want %v", c.in, got, c.want)
		}
	}
}

func TestJSONOutput(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, Config{Level: slog.LevelInfo, Format: "json"})
	log.Info("hello", "k", "v")

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if m["msg"] != "hello" || m["k"] != "v" {
		t.Errorf("unexpected log payload: %v", m)
	}
}

func TestContextAttrs(t *testing.T) {
	type ctxKey struct{}
	extract := func(ctx context.Context) []slog.Attr {
		if v, ok := ctx.Value(ctxKey{}).(string); ok {
			return []slog.Attr{slog.String("request_id", v)}
		}
		return nil
	}

	var buf bytes.Buffer
	log := New(&buf, Config{Level: slog.LevelInfo, Format: "json", Attrs: extract})

	ctx := context.WithValue(context.Background(), ctxKey{}, "abc123")
	log.InfoContext(ctx, "request handled")

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if got := m["request_id"]; got != "abc123" {
		t.Errorf("request_id attr = %v, want abc123", got)
	}
}

func TestContextAttrs_NilExtractor(t *testing.T) {
	// no Attrs func ⇒ no wrapper; logger should still work.
	var buf bytes.Buffer
	log := New(&buf, Config{Level: slog.LevelInfo, Format: "json"})
	log.InfoContext(context.Background(), "plain")
	if !bytes.Contains(buf.Bytes(), []byte(`"msg":"plain"`)) {
		t.Errorf("expected msg=plain in output, got: %s", buf.String())
	}
}

func TestContextAttrs_WithAttrsPreservesExtractor(t *testing.T) {
	// Calling .With(...) on the logger creates a derived handler; the
	// context extractor must survive that derivation.
	type ctxKey struct{}
	extract := func(ctx context.Context) []slog.Attr {
		if v, ok := ctx.Value(ctxKey{}).(string); ok {
			return []slog.Attr{slog.String("request_id", v)}
		}
		return nil
	}

	var buf bytes.Buffer
	log := New(&buf, Config{Level: slog.LevelInfo, Format: "json", Attrs: extract}).
		With("svc", "node-agent")

	ctx := context.WithValue(context.Background(), ctxKey{}, "xyz")
	log.InfoContext(ctx, "boot")

	out := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte(`"request_id":"xyz"`)) ||
		!bytes.Contains(buf.Bytes(), []byte(`"svc":"node-agent"`)) {
		t.Errorf("expected both context+with attrs; got: %s", out)
	}
}
