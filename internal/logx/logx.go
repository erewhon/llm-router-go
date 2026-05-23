// Package logx is a thin wrapper around log/slog that gives every binary
// in this repo the same logger setup: JSON or text output, a parsable
// level flag, and a hook for other packages to attach context-scoped
// attributes (e.g. request IDs from httpx) without logx importing them.
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Config controls logger construction.
type Config struct {
	Level  slog.Level
	Format string         // "json" (default) or "text"
	Source bool           // include source file:line
	Attrs  ContextAttrFunc // optional: extract attrs from request context
}

// ContextAttrFunc extracts attributes from a context. Returns nil if there
// are none. logx calls this once per log Record and prepends the attributes
// before delegating to the underlying handler.
type ContextAttrFunc func(ctx context.Context) []slog.Attr

// New returns a *slog.Logger writing to w with the given config.
func New(w io.Writer, cfg Config) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:     cfg.Level,
		AddSource: cfg.Source,
	}

	var h slog.Handler
	switch strings.ToLower(cfg.Format) {
	case "text":
		h = slog.NewTextHandler(w, opts)
	default: // "json" or unset
		h = slog.NewJSONHandler(w, opts)
	}
	if cfg.Attrs != nil {
		h = &contextHandler{Handler: h, extract: cfg.Attrs}
	}
	return slog.New(h)
}

// ParseLevel converts a string ("debug"/"info"/"warn"/"error", case-insensitive)
// to a slog.Level. An empty string is treated as "info".
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "err":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logx: unknown level %q", s)
	}
}

// contextHandler wraps an underlying slog.Handler and prepends attributes
// derived from the Record's context before delegating.
type contextHandler struct {
	slog.Handler
	extract ContextAttrFunc
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs := h.extract(ctx); len(attrs) > 0 {
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs), extract: h.extract}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name), extract: h.extract}
}
