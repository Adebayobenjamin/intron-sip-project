package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/numrange"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/store"
)

// TrafficConfigs is the store, narrowed.
type TrafficConfigs interface {
	GetTrunk(ctx context.Context, id int64, tenantID string) (store.Trunk, error)

	CreateTrafficConfig(ctx context.Context, in store.TrafficConfigCreate) (store.TrafficConfig, error)
	ListTrafficConfigs(ctx context.Context, tenantID string, trunkID int64) ([]store.TrafficConfig, error)
	GetTrafficConfig(ctx context.Context, id int64, tenantID string) (store.TrafficConfig, error)
	UpdateTrafficConfig(ctx context.Context, id int64, tenantID string, u store.TrafficConfigUpdate) (store.TrafficConfig, error)
	DeleteTrafficConfig(ctx context.Context, id int64, tenantID string) error

	Number(ctx context.Context, number, tenantID string) (store.NumberInfo, error)
	TrunkCoverage(ctx context.Context, trunkID int64) (store.Coverage, error)
}

// TrafficConfigsHandler serves lines, and the two read models built on them:
// one number's configuration, and one trunk's coverage.
type TrafficConfigsHandler struct {
	Store     TrafficConfigs
	Admission Invalidator
	Log       *slog.Logger
}

type trafficConfigRequest struct {
	TenantID      string           `json:"tenant_id"`
	TrunkID       int64            `json:"trunk_id"`
	Name          string           `json:"name"`
	Direction     string           `json:"direction"`
	DIDRanges     []numrange.Range `json:"did_ranges"`
	Status        string           `json:"status"`
	Destination   string           `json:"destination"`
	RecordingMode string           `json:"recording_mode"`
	Metadata      json.RawMessage  `json:"metadata"`
}

func (h TrafficConfigsHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}

	var req trafficConfigRequest
	if !decode(w, r, &req) {
		return
	}

	tenant, ok := scopeTenant(w, p, req.TenantID)
	if !ok {
		return
	}

	if req.TrunkID == 0 || req.Name == "" {
		fail(w, http.StatusBadRequest, "trunk_id and name are required")
		return
	}
	if req.Direction != "inbound" && req.Direction != "outbound" {
		fail(w, http.StatusBadRequest, "direction must be one of: inbound, outbound")
		return
	}
	if msg := checkLine(req.Direction, req.Destination, req.RecordingMode, req.Status); msg != "" {
		fail(w, http.StatusBadRequest, msg)
		return
	}
	if !jsonObjectOrEmpty(req.Metadata) {
		fail(w, http.StatusBadRequest, "metadata must be a JSON object")
		return
	}
	if len(req.DIDRanges) == 0 {
		fail(w, http.StatusBadRequest, "did_ranges must contain at least one block")
		return
	}
	_, bounds, err := numrange.ParseAll(req.DIDRanges)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// The trunk must be visible to the caller: their own, or the platform
	// trunk. A tenant naming another tenant's trunk gets the same 404 as a
	// trunk that does not exist.
	trunk, err := h.Store.GetTrunk(r.Context(), req.TrunkID, tenant)
	if err != nil {
		failErr(w, h.Log, "get trunk for traffic config", err)
		return
	}

	// On a client's own trunk the line is theirs. On the platform trunk the
	// line is what is LEASED, so a platform caller has to say to whom.
	if tenant == "" {
		tenant = trunk.TenantID
	}
	if tenant == "" {
		fail(w, http.StatusBadRequest,
			"tenant_id is required for a line on the platform trunk: it says which tenant the numbers are leased to")
		return
	}

	c, err := h.Store.CreateTrafficConfig(r.Context(), store.TrafficConfigCreate{
		TenantID: tenant, TrunkID: req.TrunkID, Name: req.Name,
		Direction: req.Direction, DIDRanges: bounds, Status: req.Status,
		Destination: req.Destination, RecordingMode: req.RecordingMode,
		Metadata: req.Metadata,
	})
	if err != nil {
		failErr(w, h.Log, "create traffic config", err)
		return
	}

	// A number configured a moment ago is exactly the one someone is about to
	// test, and a cached "no line" would refuse it for the negative TTL.
	invalidateAll(h.Admission)

	h.Log.Info("traffic config created", "id", c.ID, "tenant", c.TenantID, "trunk", c.TrunkID,
		"direction", c.Direction, "destination", c.Destination, "by", p.Label)
	respond(w, http.StatusCreated, c)
}

func (h TrafficConfigsHandler) List(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}
	tenant, ok := scopeTenant(w, p, r.URL.Query().Get("tenant_id"))
	if !ok {
		return
	}

	var trunkID int64
	if raw := r.URL.Query().Get("trunk_id"); raw != "" {
		id, ok := parseInt64(w, raw, "trunk_id")
		if !ok {
			return
		}
		trunkID = id
	}

	list, err := h.Store.ListTrafficConfigs(r.Context(), tenant, trunkID)
	if err != nil {
		failErr(w, h.Log, "list traffic configs", err)
		return
	}
	respond(w, http.StatusOK, list)
}

func (h TrafficConfigsHandler) Update(w http.ResponseWriter, r *http.Request) {
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
		Name          *string           `json:"name"`
		Status        *string           `json:"status"`
		Destination   *string           `json:"destination"`
		RecordingMode *string           `json:"recording_mode"`
		DIDRanges     *[]numrange.Range `json:"did_ranges"`
		Metadata      json.RawMessage   `json:"metadata"`
	}
	if !decode(w, r, &req) {
		return
	}

	// Read first: whether a destination is allowed depends on the line's
	// direction, which a PATCH cannot change and so does not carry.
	current, err := h.Store.GetTrafficConfig(r.Context(), id, tenant)
	if err != nil {
		failErr(w, h.Log, "get traffic config", err)
		return
	}
	if msg := checkLine(current.Direction, deref(req.Destination, current.Destination),
		deref(req.RecordingMode, current.RecordingMode), deref(req.Status, current.Status)); msg != "" {
		fail(w, http.StatusBadRequest, msg)
		return
	}
	if !jsonObjectOrEmpty(req.Metadata) {
		fail(w, http.StatusBadRequest, "metadata must be a JSON object")
		return
	}

	u := store.TrafficConfigUpdate{
		Name: req.Name, Status: req.Status, Destination: req.Destination,
		RecordingMode: req.RecordingMode, Metadata: req.Metadata,
	}
	if req.DIDRanges != nil {
		if len(*req.DIDRanges) == 0 {
			fail(w, http.StatusBadRequest,
				"did_ranges cannot be emptied; delete the traffic config instead")
			return
		}
		_, bounds, err := numrange.ParseAll(*req.DIDRanges)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		u.DIDRanges = &bounds
	}

	c, err := h.Store.UpdateTrafficConfig(r.Context(), id, tenant, u)
	if err != nil {
		failErr(w, h.Log, "update traffic config", err)
		return
	}

	invalidateAll(h.Admission)
	h.Log.Info("traffic config updated", "id", c.ID, "by", p.Label)
	respond(w, http.StatusOK, c)
}

func (h TrafficConfigsHandler) Delete(w http.ResponseWriter, r *http.Request) {
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

	if err := h.Store.DeleteTrafficConfig(r.Context(), id, tenant); err != nil {
		failErr(w, h.Log, "delete traffic config", err)
		return
	}

	invalidateAll(h.Admission)
	h.Log.Warn("traffic config deleted", "id", id, "by", p.Label)
	w.WriteHeader(http.StatusNoContent)
}

// Number serves GET /v1/numbers/{e164}: which trunk carries a number, and
// which line handles it in each direction.
func (h TrafficConfigsHandler) Number(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}
	tenant, ok := scopeTenant(w, p, "")
	if !ok {
		return
	}

	number := admission.Normalise(r.PathValue("e164"))
	if number == "" {
		fail(w, http.StatusBadRequest, "the number must contain digits, e.g. /v1/numbers/+2348000000100")
		return
	}

	info, err := h.Store.Number(r.Context(), number, tenant)
	if err != nil {
		failErr(w, h.Log, "look up number", err)
		return
	}
	respond(w, http.StatusOK, info)
}

// Coverage serves GET /v1/trunks/{id}/coverage: how a trunk's numbers are
// divided among lines, and which are not covered at all.
//
// A tenant may see coverage of its OWN trunks. The platform trunk's coverage
// lists every tenant's lines on it, and which numbers are unleased — platform
// business, so a tenant gets the same 404 as for a trunk it cannot see.
func (h TrafficConfigsHandler) Coverage(w http.ResponseWriter, r *http.Request) {
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

	trunk, err := h.Store.GetTrunk(r.Context(), id, tenant)
	if err != nil {
		failErr(w, h.Log, "get trunk for coverage", err)
		return
	}
	if tenant != "" && trunk.TenantID != tenant {
		fail(w, http.StatusNotFound, "not found")
		return
	}

	cov, err := h.Store.TrunkCoverage(r.Context(), id)
	if err != nil {
		failErr(w, h.Log, "trunk coverage", err)
		return
	}
	respond(w, http.StatusOK, cov)
}

// checkLine validates the parts of a line that depend on each other. Empty
// recording mode and status are allowed — they take the column defaults.
//
// An OUTBOUND line may not reject: its destination is what an answered
// customer is connected to, and "reject" would place a call only to hang up
// on the person who picked up.
func checkLine(direction, destination, recordingMode, status string) string {
	switch destination {
	case string(admission.DestinationAI), string(admission.DestinationHandoff):
	case string(admission.DestinationReject):
		if direction == "outbound" {
			return "an outbound line's destination must be ai or handoff; reject would hang up on whoever answers"
		}
	default:
		return "destination must be one of: ai, handoff, reject"
	}

	if recordingMode != "" && !admission.RecordingMode(recordingMode).Valid() {
		return "recording_mode must be one of: none, ai_only, full"
	}
	if status != "" && status != "active" && status != "inactive" {
		return "status must be one of: active, inactive"
	}
	return ""
}

// jsonObjectOrEmpty reports whether metadata is absent or a JSON object.
// Anything else — an array, a string — would be stored and then surprise
// whoever reads it expecting keys.
func jsonObjectOrEmpty(m json.RawMessage) bool {
	trimmed := bytes.TrimSpace(m)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	return trimmed[0] == '{'
}

func deref(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}
