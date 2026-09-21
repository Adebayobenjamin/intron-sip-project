package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/route"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/store"
)

// Trunks is the store, narrowed.
type Trunks interface {
	CreateTrunk(ctx context.Context, t store.Trunk) (store.Trunk, error)
	GetTrunk(ctx context.Context, id int64, tenantID string) (store.Trunk, error)
	DIDNumbersByTrunk(ctx context.Context, trunkID int64) ([]string, error)
	ListTrunks(ctx context.Context, tenantID string) ([]store.Trunk, error)
	UpdateTrunk(ctx context.Context, id int64, tenantID string, u store.TrunkUpdate) (store.Trunk, error)
	DeleteTrunk(ctx context.Context, id int64, tenantID string) error

	CreateHandoffTarget(ctx context.Context, h store.HandoffTarget) (store.HandoffTarget, error)
	ListHandoffTargets(ctx context.Context, tenantID string) ([]store.HandoffTarget, error)
	UpdateHandoffTarget(ctx context.Context, id int64, tenantID string, name, kind, destination *string, priority *int, enabled *bool) (store.HandoffTarget, error)
	DeleteHandoffTarget(ctx context.Context, id int64, tenantID string) error
}

// TrunksHandler serves trunks and handoff targets.
type TrunksHandler struct {
	Store Trunks
	Log   *slog.Logger
}

// createdTrunk carries a warning with the created row.
//
// A trunk is HALF an operation. Its signalling ranges must also reach
// carrier_sip_cidrs in Terraform and be applied, or calls from it are dropped
// at the security group with nothing logged on the edge — a trunk that looks
// perfectly configured and never receives a call.
//
// Nothing enforces the pair, so the response says so at the moment someone is
// looking, rather than leaving it to be discovered.
type createdTrunk struct {
	store.Trunk

	NextSteps []string `json:"next_steps,omitempty"`
}

func (h TrunksHandler) CreateTrunk(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}

	var req struct {
		TenantID        string   `json:"tenant_id"`
		Slug            string   `json:"slug"`
		Name            string   `json:"name"`
		Type            string   `json:"type"`
		Carrier         string   `json:"carrier"`
		Status          string   `json:"status"`
		IsDefault       bool     `json:"is_default"`
		SIPHost         string   `json:"sip_host"`
		SIPPort         int      `json:"sip_port"`
		SIPUsername     string   `json:"sip_username"`
		SIPPassword     string   `json:"sip_password"`
		SIPAuthMethod   string   `json:"sip_auth_method"`
		SignallingCIDRs []string `json:"signalling_cidrs"`
		MediaCIDRs      []string `json:"media_cidrs"`
		ChannelSize     *int     `json:"channel_size"`
		CallerIDFormat  string   `json:"caller_id_format"`
		TwilioTrunkSID  string   `json:"twilio_trunk_sid"`
	}
	if !decode(w, r, &req) {
		return
	}

	// A trunk with no tenant is the shared platform carrier, which only a
	// platform key may create.
	tenant, ok := scopeTenant(w, p, req.TenantID)
	if !ok {
		return
	}

	if req.Slug == "" || req.Name == "" || req.Carrier == "" {
		fail(w, http.StatusBadRequest, "slug, name and carrier are required")
		return
	}
	if !validTrunkType(req.Type) {
		fail(w, http.StatusBadRequest, "type must be one of: inbound, outbound, bi_directional")
		return
	}
	if !validAuthMethod(req.SIPAuthMethod) {
		fail(w, http.StatusBadRequest, "sip_auth_method must be one of: ip_whitelisting, registration")
		return
	}

	// REFUSED, not ignored. sip_password is stored encrypted at the
	// application layer and that encryption does not exist yet. Accepting the
	// field would either store a carrier credential in the clear or silently
	// drop it — and a trunk that appears to have a password but does not
	// fails at the first call, from the carrier's side, with a 403.
	if req.SIPPassword != "" {
		fail(w, http.StatusNotImplemented,
			"sip_password cannot be set yet: credential encryption is not implemented. "+
				"Use sip_auth_method=ip_whitelisting, or set the column out of band.")
		return
	}
	if req.SIPAuthMethod == "registration" {
		h.Log.Warn("registration trunk created with no credential",
			"slug", req.Slug, "by", p.Label)
	}

	t, err := h.Store.CreateTrunk(r.Context(), store.Trunk{
		TenantID: tenant, Slug: req.Slug, Name: req.Name,
		Type: req.Type, Carrier: req.Carrier, Status: req.Status,
		IsDefault: req.IsDefault,
		SIPHost:   req.SIPHost, SIPPort: req.SIPPort, SIPUsername: req.SIPUsername,
		SIPAuthMethod:   req.SIPAuthMethod,
		SignallingCIDRs: req.SignallingCIDRs, MediaCIDRs: req.MediaCIDRs,
		ChannelSize: req.ChannelSize, CallerIDFormat: req.CallerIDFormat,
		TwilioTrunkSID: req.TwilioTrunkSID,
	})
	if err != nil {
		failErr(w, h.Log, "create trunk", err)
		return
	}

	h.Log.Info("trunk created", "slug", t.Slug, "carrier", t.Carrier, "by", p.Label)

	resp := createdTrunk{Trunk: t}
	if len(t.SignallingCIDRs) > 0 {
		resp.NextSteps = append(resp.NextSteps,
			"add these signalling_cidrs to TF_VAR_carrier_sip_cidrs and apply the "+
				"sip-core stack — until then calls from this trunk are dropped at the "+
				"security group, with nothing logged on the edge")
	} else {
		resp.NextSteps = append(resp.NextSteps,
			"signalling_cidrs is empty: record the carrier's signalling ranges here "+
				"and in TF_VAR_carrier_sip_cidrs, or no call from this trunk will arrive")
	}

	respond(w, http.StatusCreated, resp)
}

func (h TrunksHandler) ListTrunks(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}
	tenant, ok := scopeTenant(w, p, r.URL.Query().Get("tenant_id"))
	if !ok {
		return
	}

	list, err := h.Store.ListTrunks(r.Context(), tenant)
	if err != nil {
		failErr(w, h.Log, "list trunks", err)
		return
	}
	respond(w, http.StatusOK, list)
}

func (h TrunksHandler) UpdateTrunk(w http.ResponseWriter, r *http.Request) {
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
		Name             *string  `json:"name"`
		Status           *string  `json:"status"`
		SIPHost          *string  `json:"sip_host"`
		SIPPort          *int     `json:"sip_port"`
		SignallingCIDRs  []string `json:"signalling_cidrs"`
		MediaCIDRs       []string `json:"media_cidrs"`
		ChannelSize      *int     `json:"channel_size"`
		ClearChannelSize bool     `json:"clear_channel_size"`
		SIPPassword      string   `json:"sip_password"`
		TwilioTrunkSID   *string  `json:"twilio_trunk_sid"`
	}
	if !decode(w, r, &req) {
		return
	}

	if req.SIPPassword != "" {
		fail(w, http.StatusNotImplemented,
			"sip_password cannot be set yet: credential encryption is not implemented")
		return
	}

	t, err := h.Store.UpdateTrunk(r.Context(), id, tenant, store.TrunkUpdate{
		Name: req.Name, Status: req.Status,
		SIPHost: req.SIPHost, SIPPort: req.SIPPort,
		SignallingCIDRs: req.SignallingCIDRs, MediaCIDRs: req.MediaCIDRs,
		ChannelSize: req.ChannelSize, ClearChannelSize: req.ClearChannelSize,
		TwilioTrunkSID: req.TwilioTrunkSID,
	})
	if err != nil {
		failErr(w, h.Log, "update trunk", err)
		return
	}

	if req.SignallingCIDRs != nil {
		// Loud, because the half that is not in this database is the half
		// that decides whether a packet arrives at all.
		h.Log.Warn("trunk signalling ranges changed; carrier_sip_cidrs must be updated and applied",
			"trunk", t.Slug, "cidrs", req.SignallingCIDRs, "by", p.Label)
	}

	respond(w, http.StatusOK, t)
}

func (h TrunksHandler) DeleteTrunk(w http.ResponseWriter, r *http.Request) {
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

	err := h.Store.DeleteTrunk(r.Context(), id, tenant)
	if errors.Is(err, store.ErrInUse) {
		// 409, not 404 or 500: the request is well formed and the row exists,
		// and the caller can fix it by repointing the DIDs first.
		fail(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		failErr(w, h.Log, "delete trunk", err)
		return
	}

	h.Log.Warn("trunk deleted", "id", id, "by", p.Label)
	w.WriteHeader(http.StatusNoContent)
}

func validTrunkType(t string) bool {
	switch t {
	case "inbound", "outbound", "bi_directional":
		return true
	}
	return false
}

func validAuthMethod(m string) bool {
	switch m {
	case "ip_whitelisting", "registration":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Handoff targets
// ---------------------------------------------------------------------------

func (h TrunksHandler) CreateHandoffTarget(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}

	var req struct {
		TenantID    string `json:"tenant_id"`
		Name        string `json:"name"`
		Kind        string `json:"kind"`
		Destination string `json:"destination"`
		Priority    int    `json:"priority"`
		Enabled     *bool  `json:"enabled"`
	}
	if !decode(w, r, &req) {
		return
	}

	tenant, ok := requireTenant(w, p, req.TenantID)
	if !ok {
		return
	}

	if req.Name == "" || req.Destination == "" {
		fail(w, http.StatusBadRequest, "name and destination are required")
		return
	}
	if req.Kind != route.KindSIPURI && req.Kind != route.KindE164 {
		fail(w, http.StatusBadRequest, "kind must be one of: sip_uri, e164")
		return
	}

	// Enabled defaults to TRUE: someone adding a handoff target means it to be
	// used, and a target that silently does nothing is a support ticket.
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	t, err := h.Store.CreateHandoffTarget(r.Context(), store.HandoffTarget{
		TenantID: tenant, Name: req.Name, Kind: req.Kind,
		Destination: req.Destination, Priority: req.Priority, Enabled: enabled,
	})
	if err != nil {
		failErr(w, h.Log, "create handoff target", err)
		return
	}

	h.Log.Info("handoff target created",
		"tenant", tenant, "destination", t.Destination, "priority", t.Priority, "by", p.Label)
	respond(w, http.StatusCreated, t)
}

func (h TrunksHandler) ListHandoffTargets(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}
	tenant, ok := scopeTenant(w, p, r.URL.Query().Get("tenant_id"))
	if !ok {
		return
	}

	list, err := h.Store.ListHandoffTargets(r.Context(), tenant)
	if err != nil {
		failErr(w, h.Log, "list handoff targets", err)
		return
	}
	respond(w, http.StatusOK, list)
}

func (h TrunksHandler) UpdateHandoffTarget(w http.ResponseWriter, r *http.Request) {
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
		Name        *string `json:"name"`
		Kind        *string `json:"kind"`
		Destination *string `json:"destination"`
		Priority    *int    `json:"priority"`
		Enabled     *bool   `json:"enabled"`
	}
	if !decode(w, r, &req) {
		return
	}

	if req.Kind != nil && *req.Kind != route.KindSIPURI && *req.Kind != route.KindE164 {
		fail(w, http.StatusBadRequest, "kind must be one of: sip_uri, e164")
		return
	}

	t, err := h.Store.UpdateHandoffTarget(r.Context(), id, tenant,
		req.Name, req.Kind, req.Destination, req.Priority, req.Enabled)
	if err != nil {
		failErr(w, h.Log, "update handoff target", err)
		return
	}
	respond(w, http.StatusOK, t)
}

func (h TrunksHandler) DeleteHandoffTarget(w http.ResponseWriter, r *http.Request) {
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

	if err := h.Store.DeleteHandoffTarget(r.Context(), id, tenant); err != nil {
		failErr(w, h.Log, "delete handoff target", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
