package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver (no cgo), same as the reqlog sink
)

// Store is the token store. SQLite on local disk, deliberately NOT the reqlog
// Postgres: reqlog is designed to be droppable (it soft-fails to NopSink and
// drops records under load), and auth must never inherit that. A local file
// also means verification survives a network partition that takes the
// accounting DB with it.
type Store struct {
	db   *sql.DB
	path string

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

const storeSchemaSQL = `
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
	if _, err := db.Exec(storeSchemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("auth: schema %s: %w", path, err)
	}
	// The store holds secret hashes; keep it off other users' reach even if
	// the parent directory is more permissive than we made it.
	if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
		db.Close()
		return nil, fmt.Errorf("auth: chmod %s: %w", path, err)
	}
	return &Store{db: db, path: path, lastSeen: map[string]time.Time{}}, nil
}

// Close releases the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Path is the store's on-disk location, for startup logging.
func (s *Store) Path() string { return s.path }

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
		`INSERT INTO pat_tokens (id, principal, label, secret_hash, scopes, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
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
		`SELECT principal, secret_hash, scopes, expires_at, revoked_at FROM pat_tokens WHERE id = ?`, id,
	).Scan(&principal, &hash, &scopes, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, ErrUnknown
	}
	if err != nil {
		return Identity{}, fmt.Errorf("auth: lookup %s: %w", id, err)
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
		_, _ = s.db.Exec(`UPDATE pat_tokens SET last_used_at = ? WHERE id = ?`, now, id)
	}()
}

// Revoke marks a token revoked. Revoking an already-revoked token is a no-op
// rather than an error — revocation is something you want to be able to do
// twice in a panic without a scary message.
func (s *Store) Revoke(id string) error {
	res, err := s.db.Exec(
		`UPDATE pat_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("auth: revoke %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Distinguish "already revoked" from "no such token".
		var exists int
		if err := s.db.QueryRow(`SELECT COUNT(1) FROM pat_tokens WHERE id = ?`, id).Scan(&exists); err != nil {
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
	rows, err := s.db.Query(q, args...)
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
