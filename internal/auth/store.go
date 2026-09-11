package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver over the pgx already used by reqlog
	_ "modernc.org/sqlite"             // pure-Go driver (no cgo), same as the reqlog sink
)

// Store is the token store, over either SQLite or Postgres.
//
// WHICH TO USE, AND WHY THE ORIGINAL ANSWER CHANGED. This started as
// SQLite-only, deliberately not the reqlog Postgres: reqlog is designed to be
// droppable (it soft-fails to NopSink and sheds records under load), and auth
// must never inherit that. That reasoning still holds — but it was reasoning
// about ONE router. The front door is an OVN load balancer over two replicas,
// and a per-node token store means a token minted on one replica is unknown to
// the other: it authenticates roughly half the time, and a revocation takes
// effect on one node only. A credential system that works 50% of the time is
// worse than no credential system, because it teaches people to retry.
//
// So Postgres is the answer for any multi-instance deployment, and the cost is
// named rather than hidden: auth gains a hard dependency on the database. When
// Postgres is unreachable, nobody authenticates. That is a real single point of
// failure, accepted knowingly (2026-09-10) as an infrastructure problem to
// solve on its own terms rather than one to work around here.
//
// SQLite remains supported and is the right choice for a single-instance
// router, for local development, and for tests — it needs no server and keeps
// verification independent of the network.
//
// What the SPOF must NOT do is look like a credential problem: see
// ErrStoreUnavailable, which is what keeps a database outage from telling every
// caller their key is invalid.
type Store struct {
	db   *sql.DB
	dial dialect
	// desc identifies the store in logs: a path for SQLite, a REDACTED DSN for
	// Postgres. Never the raw DSN — it carries the password.
	desc string

	// lastUsed debounces the LastUsedAt write. Verification is on the request
	// path, so a write per request would turn every call into a disk sync for
	// a field nobody reads at second resolution.
	mu       sync.Mutex
	lastSeen map[string]time.Time
}

// LastUsedInterval is how stale LastUsedAt may get. Chosen so "is this token
// still in use?" stays answerable while costing at most one small write per
// token per interval.
const LastUsedInterval = time.Minute

// ErrStoreUnavailable wraps any failure to REACH the store, as opposed to a
// failure of the credential itself.
//
// This distinction is the whole safety net under the Postgres SPOF. Every
// credential outcome — unknown, revoked, expired, malformed — must collapse to
// one opaque 401 at the HTTP edge, because telling a caller which one it was
// hands them an oracle. A database outage must NOT join that set: answering
// "invalid api key" when the truth is "the token database is down" sends every
// user in the fleet off to rotate credentials that were never the problem,
// during an incident, which is the worst possible moment to mislead them.
// Callers check for this and answer 503 instead.
var ErrStoreUnavailable = errors.New("auth: token store unavailable")

// dialect carries the two things that actually differ between the backends.
type dialect struct {
	name string
	// postgres numbers its placeholders; sqlite uses "?". Queries are written
	// once with "?" and rebound for Postgres, so there is one copy of every
	// statement rather than two that can drift.
	numberedParams bool
	schema         string
}

var (
	sqliteDialect = dialect{name: "sqlite", schema: schemaSQLite}
	pgDialect     = dialect{name: "postgres", numberedParams: true, schema: schemaPostgres}
)

// placeholderRe matches a bare "?" placeholder.
var placeholderRe = regexp.MustCompile(`\?`)

// rebind converts "?" placeholders to "$1, $2, ..." for Postgres and leaves
// SQLite queries untouched. The same trick sqlx uses; kept local rather than
// taking a dependency for twenty lines.
//
// Safe here because no statement in this file contains a literal "?" inside a
// string value — if one ever does, it must be parameterised rather than
// inlined, which is the rule anyway.
func (d dialect) rebind(q string) string {
	if !d.numberedParams {
		return q
	}
	n := 0
	return placeholderRe.ReplaceAllStringFunc(q, func(string) string {
		n++
		return "$" + strconv.Itoa(n)
	})
}

// The schemas differ only in column types: SQLite has no real timestamp type
// and stores whatever the driver hands it, while Postgres wants TIMESTAMPTZ so
// two routers in different zones agree on what "expired" means.
const schemaSQLite = `
CREATE TABLE IF NOT EXISTS pat_tokens (
    id           TEXT PRIMARY KEY,
    principal    TEXT NOT NULL,
    label        TEXT NOT NULL DEFAULT '',
    secret_hash  TEXT NOT NULL,
    scopes       TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMP NOT NULL,
    last_used_at TIMESTAMP,
    expires_at   TIMESTAMP,
    revoked_at   TIMESTAMP
);
CREATE INDEX IF NOT EXISTS pat_tokens_principal_idx ON pat_tokens (principal);
`

const schemaPostgres = `
CREATE TABLE IF NOT EXISTS pat_tokens (
    id           TEXT PRIMARY KEY,
    principal    TEXT NOT NULL,
    label        TEXT NOT NULL DEFAULT '',
    secret_hash  TEXT NOT NULL,
    scopes       TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS pat_tokens_principal_idx ON pat_tokens (principal);
`

// OpenStore opens (creating if absent) the token store at path.
//
// Every failure here is fatal to the caller by design — see the package note.
// A router told to use a token store it cannot read must not fall back to
// serving unauthenticated traffic.
func OpenStore(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("auth: mkdir %s: %w", dir, err)
		}
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("auth: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("auth: ping %s: %w", path, err)
	}
	if _, err := db.Exec(sqliteDialect.schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("auth: schema %s: %w", path, err)
	}
	// The store holds secret hashes; keep it off other users' reach even if
	// the parent directory is more permissive than we made it.
	if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
		db.Close()
		return nil, fmt.Errorf("auth: chmod %s: %w", path, err)
	}
	return &Store{db: db, dial: sqliteDialect, desc: path, lastSeen: map[string]time.Time{}}, nil
}

// OpenPostgresStore opens the token store on Postgres, creating the table if
// it is absent. Use this for any deployment with more than one router instance;
// see the Store doc comment for why.
//
// Fatal on failure for the same reason OpenStore is: a router told to
// authenticate must not fall back to serving unauthenticated traffic because it
// could not reach the store. The Ping is what makes a wrong DSN or an
// unreachable database a startup failure rather than a 3am mystery — sql.Open
// alone is lazy and would succeed against a database that does not exist.
//
// The pool is small but not one: unlike SQLite (a single local file, serialised
// deliberately) verification here is a network round trip on the request path,
// so a handful of connections lets concurrent requests overlap. It stays small
// because the query is one indexed point lookup and the fleet's concurrency is
// tens, not thousands.
func OpenPostgresStore(dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("auth: open postgres store: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(time.Hour)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("auth: ping postgres store %s: %w", RedactDSN(dsn), err)
	}
	if _, err := db.Exec(pgDialect.schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("auth: schema on postgres store %s: %w", RedactDSN(dsn), err)
	}
	return &Store{db: db, dial: pgDialect, desc: RedactDSN(dsn), lastSeen: map[string]time.Time{}}, nil
}

// RedactDSN strips the password from a Postgres DSN so it can be logged.
//
// Exported because the caller logs the store location at startup and must not
// have to reimplement this. Conservative by design: anything it cannot parse
// confidently collapses to a fixed string rather than risking a password
// reaching the journal.
func RedactDSN(dsn string) string {
	// postgres://user:password@host:port/db?opts  ->  postgres://user@host:port/db
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		// No credentials in the DSN (or a key=value form). Keep only up to the
		// first space so a "password=..." token cannot ride along.
		if i := strings.IndexByte(dsn, ' '); i >= 0 {
			return dsn[:i] + " ..."
		}
		return dsn
	}
	scheme := ""
	rest := dsn
	if i := strings.Index(dsn, "://"); i >= 0 {
		scheme, rest = dsn[:i+3], dsn[i+3:]
		at = strings.LastIndex(rest, "@")
		if at < 0 {
			return "postgres://<redacted>"
		}
	}
	userinfo, hostpart := rest[:at], rest[at+1:]
	if c := strings.IndexByte(userinfo, ':'); c >= 0 {
		userinfo = userinfo[:c]
	}
	// Drop the query string: it can carry a password= too.
	if q := strings.IndexByte(hostpart, '?'); q >= 0 {
		hostpart = hostpart[:q]
	}
	return scheme + userinfo + "@" + hostpart
}

// Close releases the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Path is the store's location for startup logging: a filesystem path for
// SQLite, a password-redacted DSN for Postgres.
func (s *Store) Path() string { return s.desc }

// Backend names the store's backend ("sqlite" or "postgres"), for startup
// logging — "PAT auth enabled" should say WHERE, because per-node versus shared
// is the difference between a token that always works and one that works half
// the time.
func (s *Store) Backend() string { return s.dial.name }

// Mint creates a token for principal and returns the wire token. The wire
// token is returned exactly once and is not recoverable afterwards.
func (s *Store) Mint(principal, label string, scopes []string, expires *time.Time) (wire string, tok Token, err error) {
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return "", Token{}, errors.New("auth: principal required")
	}
	if strings.HasPrefix(principal, LegacyPrincipalPrefix) {
		// Otherwise a minted token could impersonate the synthetic identity
		// used for shared keys, and "legacy traffic is now zero" would stop
		// meaning what the migration needs it to mean.
		return "", Token{}, fmt.Errorf("auth: principal may not start with %q", LegacyPrincipalPrefix)
	}
	wire, id, hash, err := Generate()
	if err != nil {
		return "", Token{}, err
	}
	tok = Token{
		ID:         id,
		Principal:  principal,
		Label:      strings.TrimSpace(label),
		SecretHash: hash,
		Scopes:     scopes,
		CreatedAt:  time.Now().UTC(),
		ExpiresAt:  expires,
	}
	_, err = s.db.Exec(
		s.dial.rebind(`INSERT INTO pat_tokens (id, principal, label, secret_hash, scopes, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`),
		tok.ID, tok.Principal, tok.Label, tok.SecretHash, encodeScopes(scopes), tok.CreatedAt, tok.ExpiresAt,
	)
	if err != nil {
		return "", Token{}, fmt.Errorf("auth: insert token: %w", err)
	}
	return wire, tok, nil
}

// Verify resolves a wire token to its identity, or returns one of the
// Err* sentinels. It is called on the request path.
//
// The lookup hits SQLite on every request rather than an in-memory cache:
// this is a local file with a single indexed row read, and the correctness
// win — revocation takes effect on the very next request, with no TTL to
// reason about — is worth more than the microseconds a cache would save at
// this router's request rate. If it ever shows up in latency profiles, add a
// short-TTL cache then, and document the revocation lag it buys.
func (s *Store) Verify(wire string) (Identity, error) {
	id, secret, err := Split(wire)
	if err != nil {
		return Identity{}, err
	}
	var (
		principal, hash, scopes string
		expires, revoked        sql.NullTime
	)
	err = s.db.QueryRow(
		s.dial.rebind(`SELECT principal, secret_hash, scopes, expires_at, revoked_at FROM pat_tokens WHERE id = ?`), id,
	).Scan(&principal, &hash, &scopes, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, ErrUnknown
	}
	if err != nil {
		// NOT ErrUnknown. A lookup that could not run says nothing about the
		// credential, and reporting it as a bad key would have the whole fleet
		// rotating tokens during a database outage. See ErrStoreUnavailable.
		return Identity{}, fmt.Errorf("%w: lookup %s: %v", ErrStoreUnavailable, id, err)
	}
	// Compare the secret before reporting revoked/expired. Otherwise a caller
	// holding only an id learns that id's lifecycle state without ever
	// proving they hold the secret.
	if !SecretMatches(secret, hash) {
		return Identity{}, ErrUnknown
	}
	tok := Token{ID: id, Principal: principal}
	if expires.Valid {
		t := expires.Time
		tok.ExpiresAt = &t
	}
	if revoked.Valid {
		t := revoked.Time
		tok.RevokedAt = &t
	}
	if err := tok.Active(time.Now()); err != nil {
		return Identity{}, err
	}
	s.touch(id)
	return Identity{Principal: principal, TokenID: id, Scopes: decodeScopes(scopes)}, nil
}

// touch records last use, debounced by LastUsedInterval and performed off the
// request path. A failed touch is intentionally silent: it costs an accuracy
// point on a reporting field and must never fail a request that already
// authenticated.
func (s *Store) touch(id string) {
	now := time.Now().UTC()
	s.mu.Lock()
	if seen, ok := s.lastSeen[id]; ok && now.Sub(seen) < LastUsedInterval {
		s.mu.Unlock()
		return
	}
	s.lastSeen[id] = now
	s.mu.Unlock()
	go func() {
		_, _ = s.db.Exec(s.dial.rebind(`UPDATE pat_tokens SET last_used_at = ? WHERE id = ?`), now, id)
	}()
}

// Revoke marks a token revoked. Revoking an already-revoked token is a no-op
// rather than an error — revocation is something you want to be able to do
// twice in a panic without a scary message.
func (s *Store) Revoke(id string) error {
	res, err := s.db.Exec(
		s.dial.rebind(`UPDATE pat_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`),
		time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("auth: revoke %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Distinguish "already revoked" from "no such token".
		var exists int
		if err := s.db.QueryRow(s.dial.rebind(`SELECT COUNT(1) FROM pat_tokens WHERE id = ?`), id).Scan(&exists); err != nil {
			return fmt.Errorf("auth: revoke %s: %w", id, err)
		}
		if exists == 0 {
			return ErrUnknown
		}
	}
	return nil
}

// List returns stored tokens, newest first. Secret hashes are included in the
// struct but callers must never render them.
func (s *Store) List(principal string) ([]Token, error) {
	q := `SELECT id, principal, label, secret_hash, scopes, created_at, last_used_at, expires_at, revoked_at
	      FROM pat_tokens`
	var args []any
	if principal != "" {
		q += ` WHERE principal = ?`
		args = append(args, principal)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.db.Query(s.dial.rebind(q), args...)
	if err != nil {
		return nil, fmt.Errorf("auth: list: %w", err)
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var (
			t                          Token
			scopes                     string
			lastUsed, expires, revoked sql.NullTime
		)
		if err := rows.Scan(&t.ID, &t.Principal, &t.Label, &t.SecretHash, &scopes,
			&t.CreatedAt, &lastUsed, &expires, &revoked); err != nil {
			return nil, fmt.Errorf("auth: list scan: %w", err)
		}
		t.Scopes = decodeScopes(scopes)
		if lastUsed.Valid {
			v := lastUsed.Time
			t.LastUsedAt = &v
		}
		if expires.Valid {
			v := expires.Time
			t.ExpiresAt = &v
		}
		if revoked.Valid {
			v := revoked.Time
			t.RevokedAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// encodeScopes stores scopes as a sorted comma-separated string. Sorted so
// the column is stable and diffable; comma-separated because the scope
// vocabulary is small and closed (see the PAT scopes task) and a JSON blob
// would be more machinery than the data deserves.
func encodeScopes(scopes []string) string {
	if len(scopes) == 0 {
		return ""
	}
	cp := append([]string(nil), scopes...)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}

func decodeScopes(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
