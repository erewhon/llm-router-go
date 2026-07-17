package reqlog

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver (no cgo) so cross-compiled release builds stay clean
)

// SQLiteSchemaSQL is the SQLite flavour of the bootstrap schema. Same columns
// as Postgres (see SchemaSQL) but SQLite types: an INTEGER AUTOINCREMENT id,
// booleans as INTEGER 0/1, and `ts` as RFC3339Nano TEXT (UTC) — human-readable
// and lexically sortable so the ts DESC index still orders chronologically.
// Idempotent: CREATE-IF-NOT-EXISTS so re-opening an existing DB is safe.
const SQLiteSchemaSQL = `
CREATE TABLE IF NOT EXISTS router_requests (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id        TEXT,
    ts                TEXT NOT NULL,
    method            TEXT NOT NULL,
    path              TEXT NOT NULL,
    model             TEXT NOT NULL,
    backend_model     TEXT,
    backend_url       TEXT,
    resolved_via      TEXT,
    api_class         TEXT,
    via_tool_proxy    INTEGER NOT NULL DEFAULT 0,
    stream            INTEGER NOT NULL DEFAULT 0,
    status            INTEGER NOT NULL,
    latency_ms        INTEGER NOT NULL,
    prompt_tokens     INTEGER,
    completion_tokens INTEGER,
    total_tokens      INTEGER,
    error             TEXT
);
CREATE INDEX IF NOT EXISTS router_requests_ts_idx ON router_requests (ts DESC);
CREATE INDEX IF NOT EXISTS router_requests_request_id_idx ON router_requests (request_id);
CREATE INDEX IF NOT EXISTS router_requests_model_idx ON router_requests (model);
`

const sqliteInsertSQL = `
INSERT INTO router_requests
  (request_id, ts, method, path, model, backend_model, backend_url, resolved_via,
   api_class, via_tool_proxy, stream, status, latency_ms,
   prompt_tokens, completion_tokens, total_tokens, error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`

// SQLiteSink writes records asynchronously to a local SQLite database. It's the
// zero-config default sink: a single-file log that needs no server, ideal for a
// laptop or a standalone deployment where standing up Postgres isn't worth it.
// Log() is non-blocking with the same drop-on-full semantics as PostgresSink;
// Close() drains the channel and closes the database.
type SQLiteSink struct {
	db        *sql.DB
	insert    *sql.Stmt
	ch        chan Record
	done      chan struct{}
	logger    *slog.Logger
	dropped   atomic.Uint64
	closeOnce sync.Once
	path      string
}

// NewSQLite opens (creating parent directories and the file as needed) a SQLite
// database at path, applies WAL + busy_timeout pragmas, bootstraps the schema,
// and starts the writer goroutine. The pool is capped at one connection so the
// single writer never races itself into SQLITE_BUSY; concurrent readers (the
// sqlite3 CLI, Grafana) open their own connections and WAL keeps them unblocked.
func NewSQLite(path string, logger *slog.Logger) (*SQLiteSink, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("reqlog: mkdir %s: %w", dir, err)
		}
	}
	// Pragmas via the DSN so they apply to every connection the pool opens.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("reqlog: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("reqlog: ping %s: %w", path, err)
	}
	if _, err := db.Exec(SQLiteSchemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("reqlog: bootstrap schema: %w", err)
	}
	stmt, err := db.Prepare(sqliteInsertSQL)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("reqlog: prepare insert: %w", err)
	}
	s := &SQLiteSink{
		db:     db,
		insert: stmt,
		ch:     make(chan Record, 256),
		done:   make(chan struct{}),
		logger: logger,
		path:   path,
	}
	go s.run()
	return s, nil
}

// Log enqueues rec for asynchronous insert. Non-blocking: if the channel is
// full, the record is dropped and a counter increments (logged in Close()).
func (s *SQLiteSink) Log(rec Record) {
	select {
	case s.ch <- rec:
	default:
		s.dropped.Add(1)
	}
}

// Close drains the channel, waits for the writer goroutine, and closes the
// database. Idempotent, so a defer Close() and an explicit Close() coexist.
func (s *SQLiteSink) Close() error {
	s.closeOnce.Do(func() {
		close(s.ch)
		<-s.done
		if d := s.dropped.Load(); d > 0 {
			s.logger.Warn("reqlog: dropped records over session", "count", d)
		}
		s.insert.Close()
		s.db.Close()
	})
	return nil
}

// Dropped returns the cumulative number of records dropped because the buffer
// was full. Exposed for tests and (eventually) metrics.
func (s *SQLiteSink) Dropped() uint64 { return s.dropped.Load() }

func (s *SQLiteSink) run() {
	defer close(s.done)
	for rec := range s.ch {
		s.insertRec(rec)
	}
}

func (s *SQLiteSink) insertRec(rec Record) {
	ts := rec.TS
	if ts.IsZero() {
		ts = time.Now()
	}
	_, err := s.insert.Exec(
		nullIfEmpty(rec.RequestID), ts.UTC().Format(time.RFC3339Nano),
		rec.Method, rec.Path, rec.Model,
		nullIfEmpty(rec.BackendModel), nullIfEmpty(rec.BackendURL),
		nullIfEmpty(rec.ResolvedVia), nullIfEmpty(rec.APIClass),
		b2i(rec.ViaToolProxy), b2i(rec.Stream), rec.Status, rec.LatencyMS,
		rec.PromptTokens, rec.CompletionTokens, rec.TotalTokens,
		nullIfEmpty(rec.Error),
	)
	if err != nil {
		s.logger.Error("reqlog: insert failed", "err", err, "model", rec.Model, "path", rec.Path)
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
