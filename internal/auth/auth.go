// Package auth implements personal access tokens (PATs) for the router's
// front door: minting, verification, and the principal identity that lets a
// request be attributed to a person rather than to an anonymous shared key.
//
// The security posture here differs deliberately from reqlog's. Request
// logging FAILS OPEN — a dropped record loses accounting, which is bad but
// not dangerous, so the router serves without it. Auth FAILS CLOSED: if a
// token store is configured and cannot be opened, the router refuses to
// start. Never make verification depend on a component designed to be
// droppable.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Token wire format: "pat_<id>_<secret>".
//
// The id travels in clear alongside the secret so verification is a single
// indexed lookup rather than a scan over every hash, and so a leaked token
// can be identified — and revoked — from a log line that never contained the
// secret half. IDPrefix is what reqlog stores.
const (
	// Prefix marks the token as ours and makes accidental pastes greppable
	// (secret scanners key off fixed prefixes).
	Prefix = "pat_"
	// idBytes is the token id: 6 bytes -> 12 hex chars. Not a secret; it only
	// needs to be collision-free, and the store's PRIMARY KEY enforces that.
	idBytes = 6
	// secretBytes is the secret half: 256 bits of CSPRNG output.
	secretBytes = 32
)

// Verification failures. They are deliberately distinguishable to the
// operator (logs, CLI) but must collapse to one opaque 401 at the HTTP edge:
// telling a caller "this token is revoked" rather than "unknown" confirms the
// token was once real, which is a small oracle we gain nothing from offering.
var (
	ErrMalformed = errors.New("auth: malformed token")
	ErrUnknown   = errors.New("auth: unknown token")
	ErrRevoked   = errors.New("auth: token revoked")
	ErrExpired   = errors.New("auth: token expired")
)

// Identity is the resolved caller: who they are and which credential proved
// it. Both halves reach reqlog — Principal answers "whose spend is this?",
// TokenID answers "which credential, so I can revoke exactly that one".
type Identity struct {
	// Principal is the user this token belongs to ("steven"), or a synthetic
	// "legacy:<fingerprint>" for a pre-PAT shared key (see LegacyIdentity).
	Principal string
	// TokenID is the token's public id half. Safe to log.
	TokenID string
	// Scopes is reserved for the follow-up scopes task. Parsed and carried
	// now so the storage shape does not have to change later; nothing
	// enforces it yet.
	Scopes []string
}

// Legacy reports whether this identity came from a pre-PAT shared key rather
// than a real token. Traffic under a legacy identity is the migration's
// progress bar: when it reaches zero the --api-keys path can be removed.
func (i Identity) Legacy() bool { return strings.HasPrefix(i.Principal, LegacyPrincipalPrefix) }

// LegacyPrincipalPrefix namespaces synthetic principals minted for the old
// flat --api-keys / $ROUTER_API_KEYS shared secrets.
const LegacyPrincipalPrefix = "legacy:"

// Token is one stored credential. The secret itself is never stored, and
// never leaves Mint.
type Token struct {
	ID         string
	Principal  string
	Label      string
	SecretHash string
	Scopes     []string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
}

// Active reports whether the token may authenticate at time now.
func (t Token) Active(now time.Time) error {
	if t.RevokedAt != nil {
		return ErrRevoked
	}
	if t.ExpiresAt != nil && !now.Before(*t.ExpiresAt) {
		return ErrExpired
	}
	return nil
}

// Generate mints a fresh id and secret and returns the full wire token
// together with the id and the hash to store. The wire token is the only time
// the secret exists in a recoverable form; the caller must show it once and
// then drop it.
func Generate() (wire, id, secretHash string, err error) {
	idRaw := make([]byte, idBytes)
	if _, err = rand.Read(idRaw); err != nil {
		return "", "", "", fmt.Errorf("auth: generate id: %w", err)
	}
	secretRaw := make([]byte, secretBytes)
	if _, err = rand.Read(secretRaw); err != nil {
		return "", "", "", fmt.Errorf("auth: generate secret: %w", err)
	}
	id = hex.EncodeToString(idRaw)
	secret := base64.RawURLEncoding.EncodeToString(secretRaw)
	return Prefix + id + "_" + secret, id, HashSecret(secret), nil
}

// HashSecret is the at-rest transform for the secret half.
//
// A plain SHA-256 rather than bcrypt/argon2, deliberately: those exist to
// make brute force expensive against LOW-ENTROPY human passwords. This secret
// is 256 bits of CSPRNG output, so there is no dictionary to run and no
// meaningful search space to slow down — a KDF would buy nothing but per-
// request CPU on the hot path. If the token format ever admits a user-chosen
// secret, this decision must be revisited.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Split parses a wire token into its id and secret halves without touching
// any store. It validates shape only.
func Split(wire string) (id, secret string, err error) {
	rest, ok := strings.CutPrefix(wire, Prefix)
	if !ok {
		return "", "", ErrMalformed
	}
	id, secret, ok = strings.Cut(rest, "_")
	if !ok || id == "" || secret == "" {
		return "", "", ErrMalformed
	}
	if len(id) != idBytes*2 {
		return "", "", ErrMalformed
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", "", ErrMalformed
	}
	return id, secret, nil
}

// SecretMatches compares a presented secret against a stored hash in constant
// time. Both are fixed-length hex digests, so the comparison leaks nothing
// through length either.
func SecretMatches(presented, storedHash string) bool {
	return subtle.ConstantTimeCompare([]byte(HashSecret(presented)), []byte(storedHash)) == 1
}

// LooksLikePAT reports whether a bearer value is shaped like one of our
// tokens. It lets the middleware route a credential to the PAT store or to
// the legacy shared-key set without treating a typo'd PAT as a shared key
// (which would produce a confusing "invalid api key" for a real user).
func LooksLikePAT(bearer string) bool { return strings.HasPrefix(bearer, Prefix) }

// LegacyIdentity builds the synthetic identity for a pre-PAT shared key. The
// fingerprint is the first 8 hex chars of the key's SHA-256 — enough to tell
// two shared keys apart in usage reports and to confirm a specific one has
// stopped being used, while never putting the key itself in the database.
func LegacyIdentity(key string) Identity {
	sum := sha256.Sum256([]byte(key))
	fp := hex.EncodeToString(sum[:])[:8]
	return Identity{Principal: LegacyPrincipalPrefix + fp, TokenID: "legacy-" + fp}
}
