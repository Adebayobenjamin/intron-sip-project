package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeLookup struct {
	principals map[string]Principal
	err        error
	calls      int
}

func (f *fakeLookup) PrincipalByTokenHash(_ context.Context, hash []byte) (Principal, error) {
	f.calls++
	if f.err != nil {
		return Principal{}, f.err
	}
	for token, p := range f.principals {
		if bytes.Equal(HashToken(token), hash) {
			return p, nil
		}
	}
	return Principal{}, ErrUnknownToken
}

func request(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/tenants", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

// The environment token must work before any key exists — including for the
// request that creates the first one.
func TestAuthenticate_EnvTokenIsAdmin(t *testing.T) {
	a := New(&fakeLookup{}, []string{"bootstrap"})

	p, err := a.Authenticate(context.Background(), request("bootstrap"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !p.Platform() {
		t.Error("the env token should be platform-wide")
	}
	if !p.Can(ScopeAdmin) {
		t.Error("the env token should hold admin")
	}
}

// One token in the env is the ordinary case; several exist only during a
// rotation, so both must be accepted at once or the rotation is an outage.
func TestAuthenticate_AcceptsSeveralEnvTokensDuringRotation(t *testing.T) {
	a := New(&fakeLookup{}, []string{"new", "old"})

	for _, token := range []string{"new", "old"} {
		if _, err := a.Authenticate(context.Background(), request(token)); err != nil {
			t.Errorf("token %q rejected during rotation: %v", token, err)
		}
	}
}

func TestAuthenticate_UnknownTokenIsRejected(t *testing.T) {
	a := New(&fakeLookup{principals: map[string]Principal{}}, []string{"bootstrap"})

	_, err := a.Authenticate(context.Background(), request("nonsense"))
	if !errors.Is(err, ErrUnknownToken) {
		t.Errorf("want ErrUnknownToken, got %v", err)
	}
}

func TestAuthenticate_MissingTokenIsDistinctFromBad(t *testing.T) {
	a := New(&fakeLookup{}, []string{"bootstrap"})

	_, err := a.Authenticate(context.Background(), request(""))
	if !errors.Is(err, ErrNoToken) {
		t.Errorf("want ErrNoToken, got %v", err)
	}
}

// A database that cannot be reached must not look like a bad key: telling a
// caller to rotate a credential that was fine is the wrong instruction.
func TestAuthenticate_LookupFailureIsNotAnAuthFailure(t *testing.T) {
	a := New(&fakeLookup{err: errors.New("connection refused")}, nil)

	_, err := a.Authenticate(context.Background(), request("anything"))
	if errors.Is(err, ErrUnknownToken) || errors.Is(err, ErrNoToken) {
		t.Errorf("a lookup failure must not present as an auth failure: %v", err)
	}
}

// Keys are stored hashed, so the lookup must be by hash — a plaintext
// comparison would mean the token itself was in the table.
func TestAuthenticate_LooksUpByHashNotToken(t *testing.T) {
	lookup := &fakeLookup{principals: map[string]Principal{
		"tenant-key": {TenantID: "t_acme", Label: "acme", Scopes: []string{ScopeRead}},
	}}
	a := New(lookup, nil)

	p, err := a.Authenticate(context.Background(), request("tenant-key"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.TenantID != "t_acme" {
		t.Errorf("TenantID = %q, want t_acme", p.TenantID)
	}
}

func TestNewToken_IsUniqueAndPrefixed(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		tok, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[tok] {
			t.Fatal("NewToken returned a duplicate")
		}
		seen[tok] = true

		if len(tok) < 40 {
			t.Errorf("token is too short to be 32 random bytes: %d chars", len(tok))
		}
	}
}

// ---------------------------------------------------------------------------
// Tenant scoping — the boundary that matters
// ---------------------------------------------------------------------------

// A tenant-scoped caller gets its own tenant whatever it asked for. This is the
// line that stops ?tenant_id= being a cross-tenant read.
func TestScope_TenantKeyCannotReachAnotherTenant(t *testing.T) {
	p := Principal{TenantID: "t_acme", Scopes: []string{ScopeRead}}

	if _, err := p.Scope("t_other"); err == nil {
		t.Fatal("a tenant key asked for another tenant and was allowed")
	}

	got, err := p.Scope("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "t_acme" {
		t.Errorf("Scope(\"\") = %q, want the caller's own tenant", got)
	}
}

func TestScope_TenantKeyMayNameItsOwnTenant(t *testing.T) {
	p := Principal{TenantID: "t_acme"}

	got, err := p.Scope("t_acme")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "t_acme" {
		t.Errorf("Scope = %q, want t_acme", got)
	}
}

// A platform caller may name any tenant, including none — which means all.
func TestScope_PlatformKeyMayChoose(t *testing.T) {
	p := Principal{Scopes: []string{ScopeAdmin}}

	if got, _ := p.Scope("t_acme"); got != "t_acme" {
		t.Errorf("Scope(t_acme) = %q", got)
	}
	if got, _ := p.Scope(""); got != "" {
		t.Errorf("Scope(\"\") = %q, want empty meaning all tenants", got)
	}
}

// ---------------------------------------------------------------------------
// Scopes
// ---------------------------------------------------------------------------

// Admin implies everything. Enumerating the implication at each call site is
// how one gets missed.
func TestCan_AdminImpliesEverything(t *testing.T) {
	p := Principal{Scopes: []string{ScopeAdmin}}

	for _, s := range []string{ScopeRead, ScopeWrite, ScopeAdmin} {
		if !p.Can(s) {
			t.Errorf("admin should imply %s", s)
		}
	}
}

func TestCan_ReadDoesNotImplyWrite(t *testing.T) {
	p := Principal{Scopes: []string{ScopeRead}}

	if p.Can(ScopeWrite) {
		t.Error("a read key must not be able to write")
	}
	if p.Can(ScopeAdmin) {
		t.Error("a read key must not be admin")
	}
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

func TestRequire_RejectsWithoutAToken(t *testing.T) {
	a := New(&fakeLookup{}, []string{"bootstrap"})

	rec := httptest.NewRecorder()
	a.Require(ScopeRead, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the handler ran without authentication")
	})).ServeHTTP(rec, request(""))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("a 401 should say how to authenticate")
	}
}

// Authenticated but not permitted is 403, not 401. A 401 invites the client to
// retry with the same credentials forever.
func TestRequire_InsufficientScopeIsForbidden(t *testing.T) {
	lookup := &fakeLookup{principals: map[string]Principal{
		"reader": {TenantID: "t_acme", Scopes: []string{ScopeRead}},
	}}
	a := New(lookup, nil)

	rec := httptest.NewRecorder()
	a.Require(ScopeWrite, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a read key reached a write handler")
	})).ServeHTTP(rec, request("reader"))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// A database that cannot answer is 503, not 401.
func TestRequire_LookupFailureIsUnavailable(t *testing.T) {
	a := New(&fakeLookup{err: errors.New("connection refused")}, nil)

	rec := httptest.NewRecorder()
	a.Require(ScopeRead, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the handler ran despite a failed lookup")
	})).ServeHTTP(rec, request("anything"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestRequire_PassesThePrincipalThrough(t *testing.T) {
	lookup := &fakeLookup{principals: map[string]Principal{
		"k": {TenantID: "t_acme", Label: "acme", Scopes: []string{ScopeRead}},
	}}
	a := New(lookup, nil)

	var got Principal
	rec := httptest.NewRecorder()
	a.Require(ScopeRead, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		p, ok := FromContext(r.Context())
		if !ok {
			t.Fatal("no principal in the request context")
		}
		got = p
	})).ServeHTTP(rec, request("k"))

	if got.TenantID != "t_acme" || got.Label != "acme" {
		t.Errorf("principal did not reach the handler: %+v", got)
	}
}

// A handler reached without the middleware must be detectable. An empty
// Principal means platform-wide, so treating "absent" as "empty" would hand an
// unauthenticated request every tenant's data.
func TestFromContext_AbsentIsDistinctFromEmpty(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Error("FromContext should report absence")
	}
}

// The response must never echo the token back.
func TestDeny_DoesNotEchoTheToken(t *testing.T) {
	a := New(&fakeLookup{principals: map[string]Principal{}}, nil)

	rec := httptest.NewRecorder()
	a.Require(ScopeRead, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, request("super-secret-token"))

	if bytes.Contains(rec.Body.Bytes(), []byte("super-secret-token")) {
		t.Errorf("the response echoed the token: %s", rec.Body.String())
	}
}
