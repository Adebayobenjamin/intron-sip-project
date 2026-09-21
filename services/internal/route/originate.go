package route

import (
	"fmt"
	"strings"
)

// OriginateParams is what an outbound dial string needs.
type OriginateParams struct {
	CallUUID string
	TenantID string
	PlanID   string

	// FromDID is the number the call presents. Validated as owned-and-active
	// before it gets here — a carrier checks that the calling number belongs
	// to the account the call arrives on.
	FromDID string

	// ToNumber is who to ring.
	ToNumber string

	// TrunkHost is the carrier's SIP host, taken from the from-DID's trunk.
	TrunkHost string

	// CallbackTarget is where FreeSWITCH opens its per-call socket once the
	// far end answers — the controller's own DNS name, the same one the
	// inbound dialplan uses.
	CallbackTarget string

	// EdgeSIPRoute is Kamailio's PRIVATE address as a SIP URI, e.g.
	// sip:10.70.0.10:5060.
	//
	// Private, not the Elastic IP: FreeSWITCH is in a private subnet and the
	// edge security group admits the farm by security-group reference, which
	// only matches a private source. The public address would leave through
	// the NAT gateway and be dropped.
	EdgeSIPRoute string
}

// Originate builds the command for `bgapi originate`.
//
// # Different from a bridge, in two ways that matter
//
// A bridge JOINS an existing channel to a new one. An originate CREATES a
// channel from nothing, so it carries the caller ID itself and there is no
// A-leg to inherit anything from.
//
// And the application at the end runs ON THE ANSWERED CHANNEL. `&socket(...)`
// means: when the far end picks up, open a connection to the controller and
// hand it this call — the same path an inbound call takes, which is why the
// controller can drive both with one code path.
//
// # origination_uuid
//
// The channel UUID is CHOSEN HERE, before the call exists, so the socket that
// arrives when the far end answers is matched deterministically. Letting
// FreeSWITCH assign one would mean correlating on caller and callee, which
// breaks the first time two calls to the same number overlap.
func Originate(p OriginateParams) (string, error) {
	switch {
	case p.CallUUID == "":
		return "", fmt.Errorf("route: originate needs a call uuid")
	case p.ToNumber == "":
		return "", fmt.Errorf("route: originate needs a destination")
	case p.FromDID == "":
		// Without it the From is FreeSWITCH's default. Some carriers reject
		// that; worse, some accept it and deliver the call presenting a number
		// the tenant does not own.
		return "", fmt.Errorf("route: originate needs a from-DID to present")
	case p.TrunkHost == "":
		return "", fmt.Errorf("route: the from-DID %s has no trunk to leave by", p.FromDID)
	case p.CallbackTarget == "":
		return "", fmt.Errorf("route: originate needs a callback target for the answered channel")
	case p.EdgeSIPRoute == "":
		// Refused rather than defaulted. A dial string without it is accepted
		// by FreeSWITCH and placed, and the call simply never reaches the
		// edge — see the note on sip_route_uri below. Failing here turns a
		// silent non-call into a queued request that says why.
		return "", fmt.Errorf("route: originate needs the edge SIP route; set EDGE_SIP_ROUTE")
	}

	vars := []string{
		// -------------------------------------------------------------------
		// THE NEXT HOP, AND WITHOUT IT THE CALL NEVER REACHES THIS PLATFORM'S
		// EDGE AT ALL.
		//
		// The Request-URI below names the CARRIER, because that is who the
		// call is ultimately for. sip_route_uri is what makes FreeSWITCH send
		// the packet to Kamailio anyway. They are two different things and
		// the distinction is the whole trick — see docs/API.md, Contract 3.
		//
		// WHY THIS IS NOT THE DIALPLAN'S JOB, unlike every other egress leg.
		// dialplan-public.xml.j2 exports sip_route_uri onto the B-leg of an
		// inbound call, and says in its own comment that it cannot cover this
		// case: an originate has no inbound A-leg, so no dialplan runs and
		// that export never executes. The two paths are not redundant.
		//
		// Missing, FreeSWITCH resolves the carrier hostname and sends the
		// INVITE straight out of the private subnet. Observed in beta:
		//
		//   the originate is accepted, bgapi returns a job id, the request row
		//   reads `placed` — and NOTHING EVER ARRIVES AT KAMAILIO. The packet
		//   leaves via the NAT gateway, so the carrier sees a source address
		//   its IP ACL does not list and answers 403; and even had it
		//   connected, RTPEngine never anchored the media, so there would be
		//   no audio.
		//
		// Every layer reports success. The only symptom is an outbound call
		// that silently does not happen.
		// -------------------------------------------------------------------
		"sip_route_uri=" + p.EdgeSIPRoute,

		"origination_uuid=" + p.CallUUID,
		"origination_caller_id_number=" + p.FromDID,

		// Marks the socket that arrives later as belonging to an outbound
		// call. Without it the controller would treat the answered channel as
		// an inbound one and try to resolve the dialled number as a DID we
		// own — which it is not.
		"intron_direction=outbound",
		"intron_tenant=" + p.TenantID,
		"intron_plan=" + p.PlanID,

		"sip_h_X-Intron-Egress=" + EgressCarrier,
		"sip_h_X-Intron-Tenant=" + p.TenantID,

		// Do not treat ringback as an answer. Without this a call that is
		// merely ringing can be reported as connected, and the socket opens on
		// a call nobody has picked up.
		"ignore_early_media=true",
	}

	clean := make([]string, 0, len(vars))
	for _, v := range vars {
		clean = append(clean, sanitiseVar(v))
	}

	return fmt.Sprintf("{%s}sofia/internal/sip:%s@%s &socket(%s async full)",
		strings.Join(clean, ","),
		p.ToNumber, p.TrunkHost, p.CallbackTarget), nil
}
