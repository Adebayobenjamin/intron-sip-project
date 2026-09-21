// Package auth authenticates callers of the control-plane API.
//
// # Two kinds of caller, one mechanism
//
// Everything presents a bearer token. What differs is where the token comes
// from:
//
//	api_keys row    issued per integration, hashed at rest, revocable
//	env token       CONTROL_PLANE_API_TOKEN, for bootstrap and for operators
//	                before there is any way to issue a key
//
// # Why not JWT yet
//
// JWT_SIGNING_KEY is provisioned and deliberately unused. Validating a token
// is the easy half; the hard half is having something that issues one, and
// there is no identity provider, no users table and no admin UI in this repo.
// Building half an identity system — self-issued tokens with no account to
// attach them to, no revocation and no audit trail — would be worse than the
// bearer keys below, which have all three.
//
// # Hashing
//
// Keys are stored as a SHA-256 of the presented token and never in the clear.
// A database dumped for a support ticket then leaks nothing usable, which is
// the whole reason the column is a hash rather than the key.
//
// SHA-256 rather than bcrypt deliberately: these are 256-bit random tokens,
// not passwords. There is no dictionary to attack and no user to be slow for,
// and a per-request bcrypt would put ~100ms on every call.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Scopes an API key can hold.
const (
	// ScopeAdmin may act across every tenant, including creating tenants.
	ScopeAdmin = "admin"

	// ScopeRead may read its own tenant's configuration and call records.
	ScopeRead = "read"

	// ScopeWrite may change its own tenant's configuration.
	ScopeWrite = "write"
)

// Errors the middleware distinguishes.
var (
	ErrNoToken      = errors.New("auth: no bearer token")
	ErrUnknownToken = errors.New("auth: token not recognised")
	ErrExpired      = errors.New("auth: token expired or revoked")
)

// Principal is who is making a request.
type Principal struct {
	// TenantID is empty for a platform-wide caller. Every handler that touches
	// tenant data must consult Scope rather than reading this directly — an
	// empty string silently matching everything is how cross-tenant leaks
	// happen.
	TenantID string

	Label  string
	Scopes []string
}

// Platform reports whether this caller acts across all tenants.
func (p Principal) Platform() bool { return p.TenantID == "" }

// Can reports whether the principal holds a scope. Admin implies everything:
// a key that may create tenants may obviously also read them, and enumerating
// that in every check invites one to be missed.
func (p Principal) Can(scope string) bool {
	for _, s := range p.Scopes {
		if s == ScopeAdmin || s == scope {
			return true
		}
	}
	return false
}

// Scope returns the tenant a request must be restricted to, and whether the
// caller may choose it.
//
// A platform caller may name any tenant, so the requested one is used as given
// — including empty, meaning all. A tenant-scoped caller gets its own tenant
// whatever it asked for, which is the line that stops `?tenant_id=` from being
// a cross-tenant read.
func (p Principal) Scope(requested string) (string, error) {
	if p.Platform() {
		return requested, nil
	}
	if requested != "" && requested != p.TenantID {
		return "", fmt.Errorf("auth: caller is scoped to tenant %s", p.TenantID)
	}
	return p.TenantID, nil
}

// Lookup resolves a hashed token to a principal. internal/store implements it.
type Lookup interface {
	PrincipalByTokenHash(ctx context.Context, hash []byte) (Principal, error)
}

// HashToken returns the value stored in api_keys.token_hash.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// NewToken mints a key: 32 random bytes, URL-safe.
//
// Returned in the clear exactly once, at creation. Only the hash is stored, so
// a key that is lost cannot be recovered — it has to be replaced, which is the
// correct and intended inconvenience.
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generating a token: %w", err)
	}
	return "ik_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// Authenticator turns a request into a Principal.
type Authenticator struct {
	lookup Lookup

	// envTokens are the CONTROL_PLANE_API_TOKEN values. A SET, not one value,
	// so a rotation can run in two passes — push the new token alongside the
	// old, restart, drop the old — rather than needing every holder to change
	// in the same instant.
	envTokens [][]byte
}

// New builds an Authenticator.
func New(lookup Lookup, envTokens []string) *Authenticator {
	a := &Authenticator{lookup: lookup}
	for _, t := range envTokens {
		if t != "" {
			a.envTokens = append(a.envTokens, HashToken(t))
		}
	}
	return a
}

// Authenticate resolves the bearer token on a request.
func (a *Authenticator) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	token, err := bearerToken(r)
	if err != nil {
		return Principal{}, err
	}

	hash := HashToken(token)

	// The environment token first: it must work before any key exists, which
	// includes the request that creates the first one.
	//
	// subtle.ConstantTimeCompare rather than bytes.Equal. These are hashes
	// rather than secrets, so the leak is small, but a comparison that returns
	// early is measurable and there is no reason to allow it.
	for _, known := range a.envTokens {
		if subtle.ConstantTimeCompare(hash, known) == 1 {
			return Principal{
				Label:  "control-plane-api-token",
				Scopes: []string{ScopeAdmin},
			}, nil
		}
	}

	p, err := a.lookup.PrincipalByTokenHash(ctx, hash)
	if err != nil {
		return Principal{}, err
	}
	return p, nil
}

func bearerToken(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", ErrNoToken
	}

	token, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		// Also accept the lowercase form: clients differ, and rejecting one
		// with "no bearer token" sends people looking for a missing header
		// that is plainly present.
		token, ok = strings.CutPrefix(h, "bearer ")
	}
	if !ok || strings.TrimSpace(token) == "" {
		return "", ErrNoToken
	}

	return strings.TrimSpace(token), nil
}
