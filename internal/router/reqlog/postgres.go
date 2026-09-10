package reqlog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaSQL is the bootstrap schema, applied on Sink construction. Idempotent:
// the table and indexes are CREATE-IF-NOT-EXISTS so re-runs are safe. The
// table is intentionally simpler than LiteLLM's: one row per request, no
// per-message detail, the columns we actually query from the dashboard.
const SchemaSQL = `
CREATE TABLE IF NOT EXISTS router_requests (
    id                BIGSERIAL PRIMARY KEY,
    request_id        TEXT,
    ts                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    method            TEXT NOT NULL,
    path              TEXT NOT NULL,
    model             TEXT NOT NULL,
    backend_model     TEXT,
    backend_url       TEXT,
    resolved_via      TEXT,
    api_class         TEXT,
    via_tool_proxy    BOOLEAN NOT NULL DEFAULT FALSE,
    stream            BOOLEAN NOT NULL DEFAULT FALSE,
    status            SMALLINT NOT NULL,
    latency_ms        INTEGER NOT NULL,
    prompt_tokens     INTEGER,
    completion_tokens INTEGER,
    total_tokens      INTEGER,
    cache_creation_input_tokens INTEGER,
    cache_read_input_tokens     INTEGER,
    prefix_hash_chain TEXT,
    role              TEXT,
    role_overflowed   BOOLEAN NOT NULL DEFAULT FALSE,
    failover_from     TEXT,
    error             TEXT,
    upstream_status   SMALLINT,
    error_class       TEXT
);
CREATE INDEX IF NOT EXISTS router_requests_ts_idx ON router_requests (ts DESC);
CREATE INDEX IF NOT EXISTS router_requests_request_id_idx ON router_requests (request_id);
CREATE INDEX IF NOT EXISTS router_requests_model_idx ON router_requests (model);

-- Migrations for tables created before the Anthropic passthrough (idempotent).
ALTER TABLE router_requests ADD COLUMN IF NOT EXISTS cache_creation_input_tokens INTEGER;
ALTER TABLE router_requests ADD COLUMN IF NOT EXISTS cache_read_input_tokens INTEGER;
ALTER TABLE router_requests ADD COLUMN IF NOT EXISTS prefix_hash_chain TEXT;

-- Migrations for tables created before semantic roles (idempotent).
ALTER TABLE router_requests ADD COLUMN IF NOT EXISTS role TEXT;
ALTER TABLE router_requests ADD COLUMN IF NOT EXISTS role_overflowed BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE router_requests ADD COLUMN IF NOT EXISTS failover_from TEXT;
CREATE INDEX IF NOT EXISTS router_requests_role_idx ON router_requests (role) WHERE role IS NOT NULL;

-- Migrations for tables created before upstream failure tracking (idempotent).
ALTER TABLE router_requests ADD COLUMN IF NOT EXISTS upstream_status SMALLINT;
ALTER TABLE router_requests ADD COLUMN IF NOT EXISTS error_class TEXT;
CREATE INDEX IF NOT EXISTS router_requests_error_class_idx ON router_requests (error_class) WHERE error_class IS NOT NULL;

-- Migrations for tables created before per-user attribution (idempotent).
ALTER TABLE router_requests ADD COLUMN IF NOT EXISTS principal TEXT;
ALTER TABLE router_requests ADD COLUMN IF NOT EXISTS token_id TEXT;
CREATE INDEX IF NOT EXISTS router_requests_principal_idx ON router_requests (principal) WHERE principal IS NOT NULL;

-- Migrations for tables created before upstream provenance (idempotent).
-- upstream_provider is the operator that served the request as the upstream
-- reported it; privacy_tolerance is the retention posture the router enforced.
-- Both indexed sparsely: only cloud requests populate them, so a partial index
-- stays small even though the table is mostly local traffic.
ALTER TABLE router_requests ADD COLUMN IF NOT EXISTS upstream_provider TEXT;
ALTER TABLE router_requests ADD COLUMN IF NOT EXISTS privacy_tolerance TEXT;
CREATE INDEX IF NOT EXISTS router_requests_upstream_provider_idx ON router_requests (upstream_provider) WHERE upstream_provider IS NOT NULL;
CREATE INDEX IF NOT EXISTS router_requests_privacy_tolerance_idx ON router_requests (privacy_tolerance) WHERE privacy_tolerance IS NOT NULL;

-- Migrations for tables created before provider-billed cost (idempotent).
-- upstream_cost_usd is what the PROVIDER billed, not a token-rate estimate;
-- cached_prompt_tokens is the part of the prompt that hit the provider cache.
-- DOUBLE PRECISION rather than NUMERIC: these are sub-cent floats straight off
-- the wire, summed for reporting, never used for settlement.
ALTER TABLE router_requests ADD COLUMN IF NOT EXISTS upstream_cost_usd DOUBLE PRECISION;
ALTER TABLE router_requests ADD COLUMN IF NOT EXISTS cached_prompt_tokens INTEGER;
`

const insertSQL = `
INSERT INTO router_requests
  (request_id, ts, method, path, model, backend_model, backend_url, resolved_via,
   api_class, via_tool_proxy, stream, status, latency_ms,
   prompt_tokens, completion_tokens, total_tokens,
   cache_creation_input_tokens, cache_read_input_tokens, prefix_hash_chain,
   role, role_overflowed, failover_from, error, upstream_status, error_class,
   principal, token_id, upstream_provider, privacy_tolerance,
   upstream_cost_usd, cached_prompt_tokens)
VALUES
  ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31)
`

// PostgresSink writes records asynchronously to a Postgres database. Log() is
// non-blocking: records go to a buffered channel and a single writer
// goroutine drains them. If the buffer fills (DB stalled, very high request
// rate) records are dropped with a WARN log rather than blocking the proxy
// path. Close() drains the channel and tears the pool down.
type PostgresSink struct {
	pool      *pgxpool.Pool
	ch        chan Record
	done      chan struct{}
	logger    *slog.Logger
	dropped   atomic.Uint64
	closeOnce sync.Once
}

// NewPostgres opens a pool, bootstraps the schema, and starts the writer
// goroutine. dsn is a libpq URI or key=value connection string. The given
// logger gets the WARN/ERROR lines; pass nil for slog.Default.
func NewPostgres(ctx context.Context, dsn string, logger *slog.Logger) (*PostgresSink, error) {
	if logger == nil {
		logger = slog.Default()
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("reqlog: connect %s: %w", RedactDSN(dsn), err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("reqlog: ping %s: %w", RedactDSN(dsn), err)
	}
	if _, err := pool.Exec(ctx, SchemaSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("reqlog: bootstrap schema: %w", err)
	}
	s := &PostgresSink{
		pool:   pool,
		ch:     make(chan Record, 256),
		done:   make(chan struct{}),
		logger: logger,
	}
	go s.run()
	return s, nil
}

// Log enqueues rec for asynchronous insert. Non-blocking: if the channel is
// full, the record is dropped and a counter increments (logged periodically
// in run()).
func (s *PostgresSink) Log(rec Record) {
	select {
	case s.ch <- rec:
	default:
		s.dropped.Add(1)
	}
}

// Close drains the channel, waits for the writer goroutine to finish, and
// tears down the connection pool. Idempotent — second and later calls are
// no-ops, so a defer Close() and an explicit Close() coexist safely.
func (s *PostgresSink) Close() error {
	s.closeOnce.Do(func() {
		close(s.ch)
		<-s.done
		if d := s.dropped.Load(); d > 0 {
			s.logger.Warn("reqlog: dropped records over session", "count", d)
		}
		s.pool.Close()
	})
	return nil
}

// Dropped returns the cumulative number of records dropped because the
// buffer was full. Exposed for tests and (eventually) metrics.
func (s *PostgresSink) Dropped() uint64 { return s.dropped.Load() }

func (s *PostgresSink) run() {
	defer close(s.done)
	for rec := range s.ch {
		s.insert(rec)
	}
}

func (s *PostgresSink) insert(rec Record) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ts := rec.TS
	if ts.IsZero() {
		ts = time.Now()
	}
	_, err := s.pool.Exec(ctx, insertSQL,
		nullIfEmpty(rec.RequestID), ts, rec.Method, rec.Path, rec.Model,
		nullIfEmpty(rec.BackendModel), nullIfEmpty(rec.BackendURL),
		nullIfEmpty(rec.ResolvedVia), nullIfEmpty(rec.APIClass),
		rec.ViaToolProxy, rec.Stream, rec.Status, rec.LatencyMS,
		rec.PromptTokens, rec.CompletionTokens, rec.TotalTokens,
		rec.CacheCreationInputTokens, rec.CacheReadInputTokens,
		nullIfEmpty(rec.PrefixHashChain),
		nullIfEmpty(rec.Role), rec.RoleOverflowed, nullIfEmpty(rec.FailoverFrom),
		nullIfEmpty(rec.Error),
		nullIfZero(rec.UpstreamStatus), nullIfEmpty(rec.ErrorClass),
		nullIfEmpty(rec.Principal), nullIfEmpty(rec.TokenID),
		nullIfEmpty(rec.UpstreamProvider), nullIfEmpty(rec.PrivacyTolerance),
		rec.UpstreamCostUSD, rec.CachedPromptTokens,
	)
	if err != nil {
		s.logger.Error("reqlog: insert failed", "err", err, "model", rec.Model, "path", rec.Path)
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullIfZero maps 0 to NULL: UpstreamStatus 0 means "the upstream never
// answered", which is absence, not a status code.
func nullIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// RedactDSN hides the password segment of a libpq URI for safe logging. It's
// best-effort and only touches the canonical `scheme://user:pass@host/...`
// form; key=value DSNs pass through unchanged.
func RedactDSN(dsn string) string {
	i := strings.Index(dsn, "://")
	if i < 0 {
		return dsn
	}
	rest := dsn[i+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return dsn
	}
	userpass := rest[:at]
	colon := strings.Index(userpass, ":")
	if colon < 0 {
		return dsn
	}
	return dsn[:i+3] + userpass[:colon] + ":***@" + rest[at+1:]
}
