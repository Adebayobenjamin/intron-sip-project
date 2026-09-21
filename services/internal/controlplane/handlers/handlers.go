package handlers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/controlplane/auth"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/store"
)

// respond writes a JSON body.
func respond(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

type apiError struct {
	Error string `json:"error"`
}

func fail(w http.ResponseWriter, code int, msg string) {
	respond(w, code, apiError{Error: msg})
}

// failErr maps a store error to a status.
//
// The default is 500 and a GENERIC message. A database error echoed to the
// client leaks column names, constraint names and sometimes row contents —
// and the detail belongs in the log, where the operator is, rather than in a
// response to whoever triggered it.
func failErr(w http.ResponseWriter, log *slog.Logger, op string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		// Also covers rows that exist but belong to another tenant. Answering
		// 403 for those would confirm they exist, which lets anyone enumerate
		// another tenant's identifiers.
		fail(w, http.StatusNotFound, "not found")
		return
	}

	log.Error("request failed", "op", op, "error", err)
	fail(w, http.StatusInternalServerError, "internal error")
}

// decode reads a JSON body.
//
// Unknown fields are REJECTED. A typo in a field name would otherwise be
// silently ignored and the caller would believe they had set something they
// had not — which for `recording` or `status` means finding out from a call
// that behaved wrongly.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// principal returns the caller, or writes a 500 and returns false.
//
// Reaching a handler with no principal is a wiring mistake, not an auth
// failure, and it must never be treated as a platform-wide caller: an empty
// tenant matches everything.
func principal(w http.ResponseWriter, r *http.Request, log *slog.Logger) (auth.Principal, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		log.Error("handler reached with no principal; auth middleware is not wired",
			"path", r.URL.Path)
		fail(w, http.StatusInternalServerError, "internal error")
		return auth.Principal{}, false
	}
	return p, true
}

// scopeTenant resolves which tenant a request may act on.
//
// A platform caller may name any tenant; a tenant-scoped one always gets its
// own, whatever the query string said. This is the single line that stops
// `?tenant_id=` being a cross-tenant read, so every handler goes through it.
func scopeTenant(w http.ResponseWriter, p auth.Principal, requested string) (string, bool) {
	tenant, err := p.Scope(requested)
	if err != nil {
		fail(w, http.StatusForbidden, err.Error())
		return "", false
	}
	return tenant, true
}

// requireTenant is scopeTenant for writes, which must always name one.
//
// A platform caller creating a resource has to say which tenant it belongs to:
// there is no sensible default, and guessing would file it under nothing.
func requireTenant(w http.ResponseWriter, p auth.Principal, requested string) (string, bool) {
	tenant, ok := scopeTenant(w, p, requested)
	if !ok {
		return "", false
	}
	if tenant == "" {
		fail(w, http.StatusBadRequest, "tenant_id is required")
		return "", false
	}
	return tenant, true
}

func pathInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := r.PathValue(name)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, name+" must be a number")
		return 0, false
	}
	return v, true
}
