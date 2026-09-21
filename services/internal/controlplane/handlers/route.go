// Package handlers holds the control plane's HTTP surface: parse a request,
// call something, write a response. No business logic lives here.
package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
)

// Route serves GET /v1/route.
//
// # This is an operator endpoint, not a call-path one
//
// Kamailio used to call this on every inbound INVITE. It does not any more —
// admission happens inside esl-controller, in-process, against Postgres. What
// survives is the question an operator asks first when a client says calls are
// not arriving: *who owns this number, and would a call to it be admitted
// right now?*
//
// It answers from the same code the call path uses, deliberately. An
// diagnostic endpoint with its own reimplementation of the rules tells you
// what a second implementation thinks, which is worth nothing on the day the
// two disagree.
//
// Note it answers from THIS process's cache, not the controller's. The two
// expire independently, so a very recent edit can show here before it reaches
// a live call. That is the documented staleness budget, and the response says
// so rather than leaving it to be discovered.
type Route struct {
	Admission *admission.Service
	Log       *slog.Logger
}

type routeResponse struct {
	Number      string `json:"number"`
	Admitted    bool   `json:"admitted"`
	Reason      string `json:"reason"`
	TenantID    string `json:"tenant_id,omitempty"`
	PlanID      string `json:"plan_id,omitempty"`
	Destination string `json:"destination,omitempty"`
	Recording   bool   `json:"recording"`
	TrunkID     int64  `json:"trunk_id,omitempty"`

	// Stale reports that the answer came from an expired cache entry because
	// the database could not be reached.
	Stale bool `json:"stale,omitempty"`

	// Note explains a subtlety the caller would otherwise have to know. Only
	// set when there is one.
	Note string `json:"note,omitempty"`
}

func (h Route) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")
	if did == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "did is required, e.g. /v1/route?did=+4930123456",
		})
		return
	}

	number := admission.Normalise(did)

	v, err := h.Admission.Resolve(r.Context(), did)
	if err != nil {
		// The lookup failed — the database is unreachable and nothing was
		// cached. 503 rather than 200-with-a-refusal: an outage must not read
		// as "this number does not exist", which is what someone would act on.
		h.Log.Error("admission lookup failed", "did", number, "error", err)
		writeJSON(w, http.StatusServiceUnavailable, routeResponse{
			Number:   number,
			Admitted: false,
			Reason:   string(admission.ReasonLookupFailed),
		})
		return
	}

	resp := routeResponse{
		Number:      number,
		Admitted:    v.Admitted,
		Reason:      string(v.Reason),
		TenantID:    v.TenantID,
		PlanID:      v.PlanID,
		Destination: string(v.Destination),
		Recording:   v.Recording,
		TrunkID:     v.TrunkID,
		Stale:       v.Stale,
	}

	if v.Stale {
		resp.Note = "served from an expired cache entry; the database was unreachable"
	} else {
		resp.Note = "answered from this process's cache; the controller's cache expires " +
			"independently, so a very recent edit may not have reached a live call yet"
	}

	// Always 200 for a decision, including a refusal. A number nobody owns is
	// the system working, and a 404 here would be ambiguous with "no such
	// endpoint".
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
