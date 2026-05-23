// Package httpx provides shared HTTP middleware and server helpers used by
// every binary in this repo: request ID propagation, structured access
// logs, panic recovery, and graceful shutdown.
package httpx

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"
)

// HeaderRequestID is the canonical header for request correlation. Both
// inbound (caller-provided) and outbound (set on every response).
const HeaderRequestID = "X-Request-ID"

// requestIDMaxLen caps caller-supplied IDs to defang absurd values.
const requestIDMaxLen = 128

type ctxKeyRequestID struct{}

// WithRequestID returns a context with the given request ID attached.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID{}, id)
}

// RequestIDFromContext returns the request ID in ctx, or "" if absent.
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID{}).(string)
	return v
}

// LogAttrsFromContext is a logx.ContextAttrFunc that adds request_id.
// Wire it into logx.Config.Attrs to get automatic request correlation
// on every log line emitted with the request's context.
func LogAttrsFromContext(ctx context.Context) []slog.Attr {
	if id := RequestIDFromContext(ctx); id != "" {
		return []slog.Attr{slog.String("request_id", id)}
	}
	return nil
}

// RequestID ensures every request carries an X-Request-ID. If the caller
// supplied one (up to requestIDMaxLen chars) it is preserved; otherwise a
// fresh 16-hex-char (8-byte) ID is generated. The value is stored in the
// request context and echoed on the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if id == "" || len(id) > requestIDMaxLen {
			id = newRequestID()
		}
		w.Header().Set(HeaderRequestID, id)
		ctx := WithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is extraordinary; fall back to a time-based ID.
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// AccessLog logs one structured INFO line per request: method, path,
// status, duration, response bytes. Use after RequestID so request_id
// gets attached via the context-aware logger.
func AccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r)
			logger.InfoContext(r.Context(), "http",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rw.status),
				slog.Int("bytes", rw.size),
				slog.Duration("dur", time.Since(start)),
				slog.String("remote", r.RemoteAddr),
			)
		})
	}
}

// Recover catches panics from downstream handlers, logs them with stack,
// and returns 500 if no response has been written yet.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.ErrorContext(r.Context(), "panic",
						slog.Any("err", rec),
						slog.String("stack", string(debug.Stack())),
					)
					// best-effort 500; ignore failure if headers already sent.
					http.Error(w, "internal error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Chain applies middlewares to h in the given order. The first middleware
// in the list wraps the outermost layer (runs first on the way in, last
// on the way out). Read Chain(h, A, B, C) as A(B(C(h))).
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// ServeContext runs srv until ctx is cancelled. On cancellation it calls
// srv.Shutdown with shutdownTimeout; if srv exits on its own first, its
// error is returned (with ErrServerClosed normalised to nil).
func ServeContext(ctx context.Context, srv *http.Server, shutdownTimeout time.Duration) error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("httpx: shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// ---------------------------------------------------------------------------
// responseWriter: status/size capture, with Flusher/Hijacker passthrough.
// Flusher is critical for SSE streaming.
// ---------------------------------------------------------------------------

type responseWriter struct {
	http.ResponseWriter
	status      int
	size        int
	wroteHeader bool
}

func (w *responseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		// http.ResponseWriter implicitly writes 200 on first Write;
		// record it so the access log matches reality.
		w.wroteHeader = true
	}
	n, err := w.ResponseWriter.Write(b)
	w.size += n
	return n, err
}

// Flush forwards to the underlying writer's Flusher if it implements one.
// Required for SSE pass-through to behave correctly through middleware.
func (w *responseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying writer's Hijacker if it implements one.
func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("httpx: ResponseWriter does not support Hijack")
}
