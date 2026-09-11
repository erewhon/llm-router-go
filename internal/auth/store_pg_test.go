package auth

import (
	"os"
	"strings"
	"testing"
	"time"
)

// rebind is one of those twenty-line functions where an off-by-one is silent
// until a query fails in production, so it gets its own table.
func TestRebind(t *testing.T) {
	cases := []struct {
		name, in, wantPG string
	}{
		{"none", `SELECT 1 FROM t`, `SELECT 1 FROM t`},
		{"one", `SELECT a FROM t WHERE id = ?`, `SELECT a FROM t WHERE id = $1`},
		{"several", `INSERT INTO t (a,b,c) VALUES (?, ?, ?)`, `INSERT INTO t (a,b,c) VALUES ($1, $2, $3)`},
		{
			"numbering follows position, not appearance order of clauses",
			`UPDATE t SET x = ? WHERE id = ? AND y IS NULL`,
			`UPDATE t SET x = $1 WHERE id = $2 AND y IS NULL`,
		},
		{"seven, so $10 is never reached but double digits would work", `VALUES (?,?,?,?,?,?,?)`, `VALUES ($1,$2,$3,$4,$5,$6,$7)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pgDialect.rebind(tc.in); got != tc.wantPG {
				t.Errorf("postgres rebind = %q, want %q", got, tc.wantPG)
			}
			// SQLite must be left exactly alone.
			if got := sqliteDialect.rebind(tc.in); got != tc.in {
				t.Errorf("sqlite rebind = %q, want it unchanged", got)
			}
		})
	}
}

func TestRebindHandlesDoubleDigits(t *testing.T) {
	// The real INSERT has 7 params today, but nothing stops a column being
	// added; make sure $10+ is produced correctly rather than "$1" + "0".
	in := strings.Repeat("?,", 12)
	got := pgDialect.rebind(in)
	for _, want := range []string{"$9,", "$10,", "$11,", "$12,"} {
		if !strings.Contains(got, want) {
			t.Errorf("rebind(%q) = %q, missing %q", in, got, want)
		}
	}
}

// RedactDSN exists so a password never reaches the journal. A miss here is a
// credential leak, so the assertion is "the secret is absent", not "the shape
// looks right".
func TestRedactDSNNeverLeaksThePassword(t *testing.T) {
	const secret = "sup3r-s3cret-pw"
	cases := []string{
		"postgres://router:" + secret + "@10.115.0.64:5432/router?sslmode=disable",
		"postgres://router:" + secret + "@127.0.0.1:5433/router",
		"postgresql://user:" + secret + "@host/db?password=" + secret,
		"host=10.115.0.64 port=5432 user=router password=" + secret + " dbname=router",
	}
	for _, dsn := range cases {
		got := RedactDSN(dsn)
		if strings.Contains(got, secret) {
			t.Errorf("RedactDSN(%q) leaked the password: %q", dsn, got)
		}
	}
}

func TestRedactDSNKeepsEnoughToBeUseful(t *testing.T) {
	// A redaction that hides the host too would make the startup log useless
	// for "which database did it actually open?".
	got := RedactDSN("postgres://router:pw@10.115.0.64:5432/router?sslmode=disable")
	for _, want := range []string{"router@", "10.115.0.64:5432"} {
		if !strings.Contains(got, want) {
			t.Errorf("RedactDSN = %q, want it to keep %q", got, want)
		}
	}
}

func TestSQLiteStoreReportsItsBackend(t *testing.T) {
	// The startup log leans on this to warn about per-node stores.
	st, err := OpenStore(t.TempDir() + "/pat.db")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.Close()
	if st.Backend() != "sqlite" {
		t.Errorf("Backend() = %q, want sqlite", st.Backend())
	}
}

// TestPostgresStore_RoundTrip exercises the real driver against a real
// Postgres, including the rebound statements — the SQLite tests cannot catch a
// placeholder bug. Skipped unless ROUTER_PAT_PG_DSN is set, e.g.
//
//	ROUTER_PAT_PG_DSN=postgres://postgres:test@127.0.0.1:5439/postgres \
//	    go test ./internal/auth/...
func TestPostgresStore_RoundTrip(t *testing.T) {
	dsn := os.Getenv("ROUTER_PAT_PG_DSN")
	if dsn == "" {
		t.Skip("ROUTER_PAT_PG_DSN not set; skipping Postgres integration test")
	}
	st, err := OpenPostgresStore(dsn)
	if err != nil {
		t.Fatalf("OpenPostgresStore: %v", err)
	}
	defer st.Close()
	if st.Backend() != "postgres" {
		t.Errorf("Backend() = %q, want postgres", st.Backend())
	}

	principal := "test-" + time.Now().UTC().Format("150405.000000")
	wire, tok, err := st.Mint(principal, "integration", []string{"models:local"}, nil)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	t.Cleanup(func() { _, _ = st.db.Exec(`DELETE FROM pat_tokens WHERE principal = $1`, principal) })

	id, err := st.Verify(wire)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.Principal != principal || id.TokenID != tok.ID {
		t.Errorf("Verify = %+v, want principal %q id %q", id, principal, tok.ID)
	}
	if len(id.Scopes) != 1 || id.Scopes[0] != "models:local" {
		t.Errorf("Scopes = %v, want [models:local] — scopes must survive the round trip", id.Scopes)
	}

	// List is the other rebound query with a conditional WHERE.
	toks, err := st.List(principal)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(toks) != 1 || toks[0].ID != tok.ID {
		t.Fatalf("List returned %d tokens, want the one just minted", len(toks))
	}

	if err := st.Revoke(tok.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := st.Verify(wire); err != ErrRevoked {
		t.Errorf("Verify after revoke = %v, want ErrRevoked", err)
	}
}

// A store whose database has gone away must report ErrStoreUnavailable, NOT
// ErrUnknown. This is the contract the 503 path depends on; if it regresses,
// a database outage silently becomes "invalid api key" for every caller.
func TestVerifyReportsStoreUnavailableWhenTheDBIsGone(t *testing.T) {
	st, err := OpenStore(t.TempDir() + "/pat.db")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	wire, _, err := st.Mint("steven", "x", nil, nil)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Closing the pool is the cheapest faithful stand-in for "the database is
	// unreachable": every subsequent query fails at the driver.
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err = st.Verify(wire)
	if err == nil {
		t.Fatal("Verify succeeded against a closed store")
	}
	if !isStoreUnavailable(err) {
		t.Errorf("Verify error = %v, want it to wrap ErrStoreUnavailable", err)
	}
	// And it must NOT look like a credential problem.
	if err == ErrUnknown || err == ErrRevoked || err == ErrExpired || err == ErrMalformed {
		t.Errorf("Verify error = %v, which the HTTP edge would turn into a 401", err)
	}
}

func isStoreUnavailable(err error) bool {
	for e := err; e != nil; {
		if e == ErrStoreUnavailable {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}
