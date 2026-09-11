package main

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/erewhon/llm-router-go/internal/auth"
)

// PAT admin modes: mint, list and revoke personal access tokens without
// starting a server. Deliberately part of the router binary rather than a
// second command — the token store is the router's own state, and a separate
// binary would be one more artifact to deploy, version and keep in step.
//
// Exit codes: 0 success, 1 operation failed, 2 usage error.

type patAdminOpts struct {
	dbPath string
	// dsn selects the shared Postgres store. Mutually exclusive with dbPath,
	// and it must be supported here rather than only on the serving path:
	// a store you cannot mint into is not a store.
	dsn     string
	mint    bool
	list    bool
	revoke  string
	user    string
	label   string
	expires time.Duration
	stdout  io.Writer
	stderr  io.Writer
}

func runPATAdmin(o patAdminOpts) int {
	switch {
	case o.dsn != "" && o.dbPath != "":
		fmt.Fprintln(o.stderr, "--pat-dsn and --pat-db are mutually exclusive; they are different stores holding different tokens")
		return 2
	case o.dsn == "" && o.dbPath == "":
		fmt.Fprintln(o.stderr, "--pat-dsn or --pat-db is required for PAT admin commands")
		return 2
	}
	// Exactly one mode. Silently preferring one over another would make a
	// typo'd invocation look like it worked.
	n := 0
	for _, on := range []bool{o.mint, o.list, o.revoke != ""} {
		if on {
			n++
		}
	}
	if n > 1 {
		fmt.Fprintln(o.stderr, "choose exactly one of --pat-mint, --pat-list, --pat-revoke")
		return 2
	}

	var (
		store *auth.Store
		err   error
	)
	if o.dsn != "" {
		store, err = auth.OpenPostgresStore(o.dsn)
	} else {
		store, err = auth.OpenStore(o.dbPath)
	}
	if err != nil {
		// Never print o.dsn raw — it carries a password.
		fmt.Fprintf(o.stderr, "open token store: %v\n", err)
		return 1
	}
	defer store.Close()

	switch {
	case o.mint:
		return patMintCmd(store, o)
	case o.list:
		return patListCmd(store, o)
	default:
		return patRevokeCmd(store, o)
	}
}

func patMintCmd(store *auth.Store, o patAdminOpts) int {
	if o.user == "" {
		fmt.Fprintln(o.stderr, "--pat-mint requires --pat-user")
		return 2
	}
	var expires *time.Time
	if o.expires > 0 {
		t := time.Now().UTC().Add(o.expires)
		expires = &t
	}
	wire, tok, err := store.Mint(o.user, o.label, nil, expires)
	if err != nil {
		fmt.Fprintf(o.stderr, "mint: %v\n", err)
		return 1
	}
	// The secret exists in recoverable form exactly here and never again.
	// Say so plainly: a user who assumes they can look it up later will
	// discover otherwise at the worst moment.
	fmt.Fprintf(o.stdout, "token id:  %s\nprincipal: %s\n", tok.ID, tok.Principal)
	if tok.Label != "" {
		fmt.Fprintf(o.stdout, "label:     %s\n", tok.Label)
	}
	if expires != nil {
		fmt.Fprintf(o.stdout, "expires:   %s\n", expires.Format(time.RFC3339))
	}
	fmt.Fprintf(o.stdout, "\n%s\n\n", wire)
	fmt.Fprintln(o.stdout, "Copy it now — only its hash is stored, so it cannot be shown again.")
	if o.dsn != "" {
		fmt.Fprintf(o.stdout, "Revoke with: llm-router-go --pat-dsn \"$ROUTER_PG_DSN\" --pat-revoke %s\n", tok.ID)
	} else {
		fmt.Fprintf(o.stdout, "Revoke with: llm-router-go --pat-db %s --pat-revoke %s\n", o.dbPath, tok.ID)
	}
	return 0
}

func patListCmd(store *auth.Store, o patAdminOpts) int {
	toks, err := store.List(o.user)
	if err != nil {
		fmt.Fprintf(o.stderr, "list: %v\n", err)
		return 1
	}
	if len(toks) == 0 {
		fmt.Fprintln(o.stdout, "no tokens")
		return 0
	}
	tw := tabwriter.NewWriter(o.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPRINCIPAL\tLABEL\tCREATED\tLAST USED\tSTATE")
	now := time.Now()
	for _, t := range toks {
		state := "active"
		if err := t.Active(now); err != nil {
			switch {
			case errors.Is(err, auth.ErrRevoked):
				state = "revoked"
			case errors.Is(err, auth.ErrExpired):
				state = "expired"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.Principal, dash(t.Label),
			t.CreatedAt.Format("2006-01-02"), tsOrDash(t.LastUsedAt), state)
	}
	_ = tw.Flush()
	return 0
}

func patRevokeCmd(store *auth.Store, o patAdminOpts) int {
	if err := store.Revoke(o.revoke); err != nil {
		if errors.Is(err, auth.ErrUnknown) {
			fmt.Fprintf(o.stderr, "no token with id %s\n", o.revoke)
			return 1
		}
		fmt.Fprintf(o.stderr, "revoke: %v\n", err)
		return 1
	}
	fmt.Fprintf(o.stdout, "revoked %s\n", o.revoke)
	return 0
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func tsOrDash(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Format("2006-01-02 15:04")
}
