package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/route"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/store"
)

// Config is the store, narrowed to what these handlers need.
type Config interface {
	CreateTenant(ctx context.Context, t store.Tenant) (store.Tenant, error)
	ListTenants(ctx context.Context) ([]store.Tenant, error)
	GetTenant(ctx context.Context, id string) (store.Tenant, error)
	UpdateTenant(ctx context.Context, id string, name, status *string, cap *int, clearCap bool) (store.Tenant, error)
	DeleteTenant(ctx context.Context, id string) error

	CreatePlan(ctx context.Context, p store.Plan) (store.Plan, error)
	UpsertAgentConfig(ctx context.Context, tenantID, planID string, c route.AgentConfig) error
	ListPlans(ctx context.Context, tenantID string) ([]store.Plan, error)
	UpdatePlan(ctx context.Context, id, tenantID string, name, status, destination *string, recording *bool) (store.Plan, error)
	DeletePlan(ctx context.Context, id, tenantID string) error

	CreateDID(ctx context.Context, d store.DID) (store.DID, error)
	ListDIDs(ctx context.Context, tenantID string) ([]store.DID, error)
	UpdateDID(ctx context.Context, id int64, tenantID string, planID *string, trunkID *int64, status *string) (store.DID, error)
	DeleteDID(ctx context.Context, id int64, tenantID string) error
}

// Invalidator lets a write drop the cached answer it just made stale.
//
// Only this process's cache. The controller runs its own and expires on its
// own TTL, so an edit still takes up to that long to reach a live call — which
// is the documented staleness budget, not something this can fix.
type Invalidator interface {
	Invalidate(number string)
}

// ConfigHandler serves the configuration resources.
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

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

func (h ConfigHandler) CreatePlan(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}

	// The agent settings are part of the PLAN, not a separate resource. One
	// row per plan, set when the plan is created — and a plan routing to `ai`
	// with no agent config is a call that gets refused pre-answer, so the two
	// belong in one request rather than in two someone can do half of.
	var req struct {
		ID          string `json:"id"`
		TenantID    string `json:"tenant_id"`
		Name        string `json:"name"`
		Status      string `json:"status"`
		Destination string `json:"destination"`
		Recording   bool   `json:"recording"`

		AgentIdentity string `json:"agent_identity"`
		RoomTemplate  string `json:"room_template"`
	}
	if !decode(w, r, &req) {
		return
	}

	tenant, ok := requireTenant(w, p, req.TenantID)
	if !ok {
		return
	}

	if req.ID == "" || req.Name == "" {
		fail(w, http.StatusBadRequest, "id and name are required")
		return
	}
	if !validDestination(req.Destination) {
		// Checked here as well as by the database so the message names the
		// allowed values instead of quoting a constraint.
		fail(w, http.StatusBadRequest, "destination must be one of: ai, handoff, reject")
		return
	}

	// A plan routing to the AI leg needs an agent. Refusing here means the
	// failure happens where someone is looking, rather than on the first call
	// to the number — which is refused pre-answer with a temporary failure and
	// tells the caller nothing.
	if req.Destination == string(admission.DestinationAI) && req.AgentIdentity == "" {
		fail(w, http.StatusBadRequest,
			"agent_identity is required for a plan with destination=ai")
		return
	}
	if req.Destination != string(admission.DestinationAI) &&
		(req.AgentIdentity != "" || req.RoomTemplate != "") {
		fail(w, http.StatusBadRequest,
			"agent_identity and room_template only apply to destination=ai")
		return
	}

	pl, err := h.Store.CreatePlan(r.Context(), store.Plan{
		ID: req.ID, TenantID: tenant, Name: req.Name,
		Status: req.Status, Destination: req.Destination, Recording: req.Recording,
	})
	if err != nil {
		failErr(w, h.Log, "create plan", err)
		return
	}

	if req.AgentIdentity != "" {
		if err := h.Store.UpsertAgentConfig(r.Context(), tenant, pl.ID, route.AgentConfig{
			RoomTemplate:  req.RoomTemplate,
			AgentIdentity: req.AgentIdentity,
		}); err != nil {
			// The plan exists and cannot route. Say so plainly rather than
			// returning a 201 for something that will refuse every call.
			failErr(w, h.Log, "attach agent config", err)
			return
		}
	}

	h.Log.Info("plan created", "plan", pl.ID, "tenant", tenant,
		"destination", pl.Destination, "by", p.Label)
	respond(w, http.StatusCreated, pl)
}

func (h ConfigHandler) ListPlans(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}
	tenant, ok := scopeTenant(w, p, r.URL.Query().Get("tenant_id"))
	if !ok {
		return
	}

	list, err := h.Store.ListPlans(r.Context(), tenant)
	if err != nil {
		failErr(w, h.Log, "list plans", err)
		return
	}
	respond(w, http.StatusOK, list)
}

func (h ConfigHandler) UpdatePlan(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}
	tenant, ok := scopeTenant(w, p, "")
	if !ok {
		return
	}

	var req struct {
		Name        *string `json:"name"`
		Status      *string `json:"status"`
		Destination *string `json:"destination"`
		Recording   *bool   `json:"recording"`

		AgentIdentity *string `json:"agent_identity"`
		RoomTemplate  *string `json:"room_template"`
	}
	if !decode(w, r, &req) {
		return
	}

	if req.Destination != nil && !validDestination(*req.Destination) {
		fail(w, http.StatusBadRequest, "destination must be one of: ai, handoff, reject")
		return
	}

	pl, err := h.Store.UpdatePlan(r.Context(), r.PathValue("id"), tenant,
		req.Name, req.Status, req.Destination, req.Recording)
	if err != nil {
		failErr(w, h.Log, "update plan", err)
		return
	}

	if req.AgentIdentity != nil || req.RoomTemplate != nil {
		cfg := route.AgentConfig{}
		if req.AgentIdentity != nil {
			cfg.AgentIdentity = *req.AgentIdentity
		}
		if req.RoomTemplate != nil {
			cfg.RoomTemplate = *req.RoomTemplate
		}
		if err := h.Store.UpsertAgentConfig(r.Context(), pl.TenantID, pl.ID, cfg); err != nil {
			failErr(w, h.Log, "update agent config", err)
			return
		}
	}

	h.Log.Info("plan updated", "plan", pl.ID, "by", p.Label)
	respond(w, http.StatusOK, pl)
}

func (h ConfigHandler) DeletePlan(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}
	tenant, ok := scopeTenant(w, p, "")
	if !ok {
		return
	}

	if err := h.Store.DeletePlan(r.Context(), r.PathValue("id"), tenant); err != nil {
		failErr(w, h.Log, "delete plan", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validDestination(d string) bool {
	switch d {
	case string(admission.DestinationAI),
		string(admission.DestinationHandoff),
		string(admission.DestinationReject):
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// DIDs
// ---------------------------------------------------------------------------

func (h ConfigHandler) CreateDID(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}

	var req struct {
		Number   string `json:"number"`
		TenantID string `json:"tenant_id"`
		PlanID   string `json:"plan_id"`
		TrunkID  int64  `json:"trunk_id"`
		Status   string `json:"status"`
	}
	if !decode(w, r, &req) {
		return
	}

	tenant, ok := requireTenant(w, p, req.TenantID)
	if !ok {
		return
	}

	// NORMALISED ON WRITE, so the unique index means what it says and a lookup
	// cannot miss a number that is plainly in the table. The same function the
	// call path uses, deliberately — two implementations would eventually
	// disagree about what a number looks like.
	number := admission.Normalise(req.Number)
	if number == "" {
		fail(w, http.StatusBadRequest, "number must contain digits, e.g. +4930123456")
		return
	}
	if number != strings.TrimSpace(req.Number) {
		h.Log.Info("did normalised on write", "given", req.Number, "stored", number)
	}

	d, err := h.Store.CreateDID(r.Context(), store.DID{
		Number: number, TenantID: tenant, PlanID: req.PlanID,
		TrunkID: req.TrunkID, Status: req.Status,
	})
	if err != nil {
		failErr(w, h.Log, "create did", err)
		return
	}

	// Drop the negative cache entry. A number provisioned a moment ago is
	// exactly the one someone is about to test, and a cached miss would refuse
	// it for the negative TTL.
	h.invalidate(number)

	h.Log.Info("did created", "number", number, "tenant", tenant, "by", p.Label)
	respond(w, http.StatusCreated, d)
}

func (h ConfigHandler) ListDIDs(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}
	tenant, ok := scopeTenant(w, p, r.URL.Query().Get("tenant_id"))
	if !ok {
		return
	}

	list, err := h.Store.ListDIDs(r.Context(), tenant)
	if err != nil {
		failErr(w, h.Log, "list dids", err)
		return
	}
	respond(w, http.StatusOK, list)
}

func (h ConfigHandler) UpdateDID(w http.ResponseWriter, r *http.Request) {
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

	var req struct {
		PlanID  *string `json:"plan_id"`
		TrunkID *int64  `json:"trunk_id"`
		Status  *string `json:"status"`
	}
	if !decode(w, r, &req) {
		return
	}

	d, err := h.Store.UpdateDID(r.Context(), id, tenant, req.PlanID, req.TrunkID, req.Status)
	if err != nil {
		failErr(w, h.Log, "update did", err)
		return
	}

	h.invalidate(d.Number)
	h.Log.Info("did updated", "number", d.Number, "by", p.Label)
	respond(w, http.StatusOK, d)
}

func (h ConfigHandler) DeleteDID(w http.ResponseWriter, r *http.Request) {
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

	if err := h.Store.DeleteDID(r.Context(), id, tenant); err != nil {
		failErr(w, h.Log, "delete did", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h ConfigHandler) invalidate(number string) {
	if h.Admission != nil && number != "" {
		h.Admission.Invalidate(number)
	}
}
