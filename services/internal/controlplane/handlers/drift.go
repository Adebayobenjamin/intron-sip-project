package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/twilio"
)

// CarrierReader fetches a carrier's view of a trunk.
type CarrierReader interface {
	FetchTrunk(ctx context.Context, trunkSID string) (twilio.Snapshot, error)
}

// DriftHandler serves GET /v1/trunks/{id}/drift.
//
// # What this answers
//
// Four pieces of configuration live in Twilio's console with nothing checking
// them against ours, and every mismatch fails silently. The worst is the
// Origination URI: point it anywhere but our edge and NO INBOUND CALL EVER
// ARRIVES, with nothing to see on our side because the packet never reaches
// us.
//
// This endpoint is the thing to curl when a client says calls are not
// arriving, before anyone starts reading SIP traces.
//
// # Read only
//
// It never writes to Twilio. Detecting drift is most of the value; correcting
// it automatically is a much larger commitment and easy to get wrong in a
// direction that takes a trunk down.
type DriftHandler struct {
	Store   Trunks
	Carrier CarrierReader

	// EdgeSIPURI is what Twilio's origination must point at. Without it there
	// is nothing to compare against, and the endpoint says so rather than
	// reporting a pass it cannot justify.
	EdgeSIPURI string

	Log *slog.Logger
}

func (h DriftHandler) Check(w http.ResponseWriter, r *http.Request) {
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

	if h.Carrier == nil {
		fail(w, http.StatusNotImplemented,
			"Twilio credentials are not configured: set TWILIO_ACCOUNT_SID and TWILIO_AUTH_TOKEN")
		return
	}

	trunk, err := h.Store.GetTrunk(r.Context(), id, tenant)
	if err != nil {
		failErr(w, h.Log, "get trunk", err)
		return
	}

	if trunk.TwilioTrunkSID == "" {
		// Not an error: most trunks are not Twilio. Say what to do rather than
		// returning an empty report that reads like a pass.
		fail(w, http.StatusBadRequest,
			"this trunk has no twilio_trunk_sid; PATCH it with the SID from the Twilio console "+
				"(it starts TK...) before drift can be checked")
		return
	}

	dids, err := h.Store.DIDNumbersByTrunk(r.Context(), id)
	if err != nil {
		failErr(w, h.Log, "list trunk dids", err)
		return
	}

	snap, err := h.Carrier.FetchTrunk(r.Context(), trunk.TwilioTrunkSID)
	if err != nil {
		// 502, not 500: this service is fine and the upstream is not, and the
		// message is Twilio's own — which for a bad credential or an unknown
		// SID is exactly what the operator needs.
		h.Log.Error("twilio fetch failed", "trunk", trunk.Slug, "error", err)
		fail(w, http.StatusBadGateway, err.Error())
		return
	}

	report := twilio.Compare(trunk.ID, twilio.Expected{
		EdgeSIPURI:      h.EdgeSIPURI,
		SignallingCIDRs: trunk.SignallingCIDRs,
		DIDs:            dids,
	}, snap)

	if !report.OK {
		h.Log.Warn("trunk configuration has drifted from Twilio",
			"trunk", trunk.Slug, "checks", failedCheckNames(report))
	}

	// Always 200. A drifted trunk is a successful answer to the question
	// asked, and an error status would make a monitoring script treat "I
	// checked and found problems" the same as "I could not check".
	respond(w, http.StatusOK, report)
}

func failedCheckNames(r twilio.Report) []string {
	var out []string
	for _, c := range r.Checks {
		if !c.OK {
			out = append(out, c.Name)
		}
	}
	return out
}
