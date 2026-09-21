package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/controlplane/auth"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/store"
)

// Keys is the store, narrowed.
type Keys interface {
	CreateAPIKey(ctx context.Context, tenantID, label string, scopes []string, hash []byte, expiresAt *time.Time) (store.APIKey, error)
	ListAPIKeys(ctx context.Context, tenantID string) ([]store.APIKey, error)
	RevokeAPIKey(ctx context.Context, id int64, tenantID string) error
}

// KeysHandler serves /v1/api-keys.
type KeysHandler struct {
	Store Keys
	Log   *slog.Logger
}

type createdKey struct {
	store.APIKey

	// Token is returned ONCE, at creation, and never again — only its hash is
	// stored. A key that is lost has to be replaced rather than recovered,
	// which is the intended inconvenience: it is also what makes the stored
	// hash worth anything.
	Token string `json:"token"`

	Note string `json:"note"`
}

func (h KeysHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}

	var req struct {
		TenantID  string     `json:"tenant_id"`
		Label     string     `json:"label"`
		Scopes    []string   `json:"scopes"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if !decode(w, r, &req) {
		return
	}

	if req.Label == "" {
		// Required, because "which key is this and can I revoke it" is the
		// only question ever asked of this table, and an unlabelled key
		// answers neither.
		fail(w, http.StatusBadRequest, "label is required: name the integration this key is for")
		return
	}

	tenant, ok := scopeTenant(w, p, req.TenantID)
	if !ok {
		return
	}

	scopes, err := validScopes(req.Scopes, p)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	token, err := auth.NewToken()
	if err != nil {
		failErr(w, h.Log, "generate token", err)
		return
	}

	k, err := h.Store.CreateAPIKey(r.Context(), tenant, req.Label, scopes,
		auth.HashToken(token), req.ExpiresAt)
	if err != nil {
		failErr(w, h.Log, "create api key", err)
		return
	}

	// Log the creation, never the token.
	h.Log.Info("api key created",
		"id", k.ID, "label", k.Label, "tenant", tenant, "scopes", scopes, "by", p.Label)

	respond(w, http.StatusCreated, createdKey{
		APIKey: k,
		Token:  token,
		Note:   "store this token now; only its hash is kept and it cannot be shown again",
	})
}

// validScopes checks the requested scopes and refuses escalation.
//
// A caller cannot mint a key with a scope it does not itself hold. Without
// this, any admin-scoped tenant key could issue itself a platform key, and the
// tenant boundary would be advisory.
func validScopes(requested []string, p auth.Principal) ([]string, error) {
	if len(requested) == 0 {
		return []string{auth.ScopeRead}, nil
	}

	for _, s := range requested {
		switch s {
		case auth.ScopeRead, auth.ScopeWrite, auth.ScopeAdmin:
		default:
			return nil, errUnknownScope(s)
		}
		if !p.Can(s) {
			return nil, errEscalation(s)
		}
	}
	return requested, nil
}

func (h KeysHandler) List(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}
	tenant, ok := scopeTenant(w, p, r.URL.Query().Get("tenant_id"))
	if !ok {
		return
	}

	keys, err := h.Store.ListAPIKeys(r.Context(), tenant)
	if err != nil {
		failErr(w, h.Log, "list api keys", err)
		return
	}
	respond(w, http.StatusOK, keys)
}

func (h KeysHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}
	tenant, ok := scopeTenant(w, p, "")
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}

	if err := h.Store.RevokeAPIKey(r.Context(), id, tenant); err != nil {
		failErr(w, h.Log, "revoke api key", err)
		return
	}

	h.Log.Warn("api key revoked", "id", id, "by", p.Label)
	w.WriteHeader(http.StatusNoContent)
}

type scopeError struct{ msg string }

func (e scopeError) Error() string { return e.msg }

func errUnknownScope(s string) error {
	return scopeError{"unknown scope " + s + "; valid scopes are read, write, admin"}
}

func errEscalation(s string) error {
	return scopeError{"cannot issue a key with the " + s + " scope: this key does not hold it"}
}
