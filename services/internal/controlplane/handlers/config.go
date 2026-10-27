package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/store"
)

// Config is the store, narrowed to what these handlers need.
type Config interface {
	CreateTenant(ctx context.Context, t store.Tenant) (store.Tenant, error)
	ListTenants(ctx context.Context) ([]store.Tenant, error)
	GetTenant(ctx context.Context, id string) (store.Tenant, error)
	UpdateTenant(ctx context.Context, id string, name, status *string, cap *int, clearCap bool) (store.Tenant, error)
	DeleteTenant(ctx context.Context, id string) error
}

// Invalidator lets a write drop the cached answers it just made stale.
//
// Only this process's cache. The controller runs its own and expires on its
// own TTL, so an edit still takes up to that long to reach a live call — which
// is the documented staleness budget, not something this can fix.
//
// Everything, not one number: a change to a trunk or a line moves whole
// blocks, and working out which cached numbers fall inside them costs more
// than refilling a cache that warms itself on the next call.
type Invalidator interface {
	InvalidateAll()
}

// ConfigHandler serves tenants.
type ConfigHandler struct {
	Store     Config
	Admission Invalidator
	Log       *slog.Logger
}

// ---------------------------------------------------------------------------
// Tenants
// ---------------------------------------------------------------------------

type tenantRequest struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	ConcurrencyCap *int   `json:"concurrency_cap"`
}

func (h ConfigHandler) CreateTenant(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}

	// Creating a tenant is inherently cross-tenant: a tenant-scoped key
	// creating another tenant would be a privilege escalation.
	if !p.Platform() {
		fail(w, http.StatusForbidden, "only a platform key may create tenants")
		return
	}

	var req tenantRequest
	if !decode(w, r, &req) {
		return
	}

	if req.ID == "" || req.Name == "" {
		fail(w, http.StatusBadRequest, "id and name are required")
		return
	}
	if req.ConcurrencyCap != nil && *req.ConcurrencyCap <= 0 {
		// A cap of zero would admit nothing while looking like a limit. Say so
		// rather than letting the check constraint answer with a constraint
		// name.
		fail(w, http.StatusBadRequest, "concurrency_cap must be positive, or omitted for no cap")
		return
	}

	t, err := h.Store.CreateTenant(r.Context(), store.Tenant{
		ID: req.ID, Name: req.Name, Status: req.Status, ConcurrencyCap: req.ConcurrencyCap,
	})
	if err != nil {
		failErr(w, h.Log, "create tenant", err)
		return
	}

	h.Log.Info("tenant created", "tenant", t.ID, "by", p.Label)
	respond(w, http.StatusCreated, t)
}

func (h ConfigHandler) ListTenants(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}

	// A tenant-scoped key sees exactly itself, not the list.
	if !p.Platform() {
		t, err := h.Store.GetTenant(r.Context(), p.TenantID)
		if err != nil {
			failErr(w, h.Log, "get tenant", err)
			return
		}
		respond(w, http.StatusOK, []store.Tenant{t})
		return
	}

	list, err := h.Store.ListTenants(r.Context())
	if err != nil {
		failErr(w, h.Log, "list tenants", err)
		return
	}
	respond(w, http.StatusOK, list)
}

func (h ConfigHandler) UpdateTenant(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}

	id := r.PathValue("id")
	if _, ok := scopeTenant(w, p, id); !ok {
		return
	}

	var req struct {
		Name           *string `json:"name"`
		Status         *string `json:"status"`
		ConcurrencyCap *int    `json:"concurrency_cap"`
		ClearCap       bool    `json:"clear_concurrency_cap"`
	}
	if !decode(w, r, &req) {
		return
	}

	t, err := h.Store.UpdateTenant(r.Context(), id, req.Name, req.Status, req.ConcurrencyCap, req.ClearCap)
	if err != nil {
		failErr(w, h.Log, "update tenant", err)
		return
	}

	// Suspending a tenant must stop its calls promptly, and this process's
	// cached admission answers would otherwise keep admitting them for a TTL.
	invalidateAll(h.Admission)
	h.Log.Info("tenant updated", "tenant", id, "by", p.Label)
	respond(w, http.StatusOK, t)
}

func (h ConfigHandler) DeleteTenant(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}
	if !p.Platform() {
		fail(w, http.StatusForbidden, "only a platform key may delete tenants")
		return
	}

	if err := h.Store.DeleteTenant(r.Context(), r.PathValue("id")); err != nil {
		failErr(w, h.Log, "delete tenant", err)
		return
	}

	h.Log.Warn("tenant deleted", "tenant", r.PathValue("id"), "by", p.Label)
	w.WriteHeader(http.StatusNoContent)
}

// invalidateAll drops this process's cached admission answers after a write
// that may have changed them.
func invalidateAll(inv Invalidator) {
	if inv != nil {
		inv.InvalidateAll()
	}
}
