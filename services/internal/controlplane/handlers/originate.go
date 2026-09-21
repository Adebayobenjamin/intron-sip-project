package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/store"
)

// Originate is the store, narrowed.
type Originate interface {
	ResolveFromDID(ctx context.Context, number, tenantID string) (trunkID int64, trunkHost string, err error)
	ClaimCall(ctx context.Context, c calls.Claim) error
	QueueOriginate(ctx context.Context, r calls.OriginateRequest) (calls.OriginateRequest, error)
	GetOriginate(ctx context.Context, callUUID, tenantID string) (calls.OriginateRequest, error)
}

// UUIDs generates call identifiers.
type UUIDs interface {
	New() string
}

// OriginateHandler serves POST /v1/originate.
//
// # What it does, and what it deliberately does not
//
// It validates, claims a concurrency slot, and QUEUES the call. It does not
// dial: only the controller holds ESL connections. The response is 202 with a
// call_uuid, because the call genuinely has not happened yet — the phone has
// not even rung.
type OriginateHandler struct {
	Store OriginateStore
	UUID  UUIDs
	Log   *slog.Logger
}

// OriginateStore is the full set this handler needs.
type OriginateStore interface {
	Originate
}

type originateResponse struct {
	calls.OriginateRequest

	Note string `json:"note,omitempty"`
}

func (h OriginateHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}

	var req struct {
		TenantID string `json:"tenant_id"`
		From     string `json:"from"`
		To       string `json:"to"`
		PlanID   string `json:"plan_id"`
	}
	if !decode(w, r, &req) {
		return
	}

	tenant, ok := requireTenant(w, p, req.TenantID)
	if !ok {
		return
	}

	// Normalised with the same function the call path uses, so a number given
	// as 0049… matches the row stored as +49… — and so the DID lookup below
	// cannot miss a number that is plainly provisioned.
	from := admission.Normalise(req.From)
	to := admission.Normalise(req.To)

	if from == "" || to == "" {
		fail(w, http.StatusBadRequest, "from and to are required, in E.164 (e.g. +4930123456)")
		return
	}

	// THE AUTHORISATION CHECK, and it is this one line.
	//
	// The from-DID must be active and owned by the tenant. Without it, anyone
	// who can reach this endpoint can present any number on the platform —
	// including another tenant's — and caller-ID validation that trusts its
	// caller is not validation.
	//
	// The same row supplies the trunk, which makes a caller-ID/trunk mismatch
	// unrepresentable rather than merely unlikely: carriers verify the calling
	// number belongs to the account the call arrives on.
	trunkID, trunkHost, err := h.Store.ResolveFromDID(r.Context(), from, tenant)
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusForbidden,
			"the from number is not an active DID belonging to this tenant")
		return
	}
	if err != nil {
		failErr(w, h.Log, "resolve from-did", err)
		return
	}

	if trunkHost == "" {
		// The DID exists but has no trunk to leave by. Caught here rather than
		// by the controller, where it would fail after the request was
		// accepted and the caller had been told 202.
		fail(w, http.StatusConflict,
			"the from-DID has no trunk with a SIP host; set trunk_id on the DID first")
		return
	}

	callUUID := h.UUID.New()

	// Claimed BEFORE queueing, so a tenant at its cap cannot fill a queue with
	// calls it may not place. The controller does not claim again when the
	// answered socket arrives — that would count the same call twice.
	claim := calls.Claim{
		UUID:      callUUID,
		TenantID:  tenant,
		PlanID:    req.PlanID,
		TrunkID:   trunkID,
		Direction: calls.DirectionOutbound,
		DID:       from,
		Callee:    to,
	}

	if err := h.Store.ClaimCall(r.Context(), claim); err != nil {
		if errors.Is(err, calls.ErrAtCapacity) {
			// 429, not 503: this is the tenant's own limit, and the caller can
			// act on it by waiting or by raising the cap.
			fail(w, http.StatusTooManyRequests,
				"this tenant is at its concurrency cap")
			return
		}
		failErr(w, h.Log, "claim slot for originate", err)
		return
	}

	queued, err := h.Store.QueueOriginate(r.Context(), calls.OriginateRequest{
		CallUUID: callUUID,
		TenantID: tenant,
		PlanID:   req.PlanID,
		FromDID:  from,
		ToNumber: to,
		TrunkID:  trunkID,
	})
	if err != nil {
		failErr(w, h.Log, "queue originate", err)
		return
	}

	h.Log.Info("outbound call queued",
		"call_uuid", callUUID, "tenant", tenant, "to", to, "from", from, "by", p.Label)

	// 202, not 201. Nothing has been created that the caller can use yet and
	// nobody's phone has rung; a controller will pick the row up within a
	// fraction of a second.
	respond(w, http.StatusAccepted, originateResponse{
		OriginateRequest: queued,
		Note:             "queued; poll GET /v1/originate/" + callUUID + " for status",
	})
}

// Get reports what happened to a request.
//
// `placed` means DIALLED, not answered. Whether anyone picked up is a question
// for /v1/call-history, which is where the call itself ends up.
func (h OriginateHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}
	tenant, ok := scopeTenant(w, p, "")
	if !ok {
		return
	}

	req, err := h.Store.GetOriginate(r.Context(), r.PathValue("call_uuid"), tenant)
	if err != nil {
		failErr(w, h.Log, "get originate", err)
		return
	}
	respond(w, http.StatusOK, req)
}
