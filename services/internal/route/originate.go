package route

import (
	"fmt"
	"strconv"
	"strings"
)

// TransferContext is the FreeSWITCH dialplan context a transferred call lands
// in. It is the name of the context in
// infra/ansible/roles/freeswitch/templates/dialplan-public.xml.j2 — the one
// inbound calls arrive in, whose catch-all hands every call to the controller.
const TransferContext = "public"

// OriginateParams is what an outbound dial string needs.
type OriginateParams struct {
	CallUUID string
	TenantID string

	// TrafficConfigID is the outbound line the from-number belongs to. The
	// answered socket reads it back to know what to connect the customer to.
	TrafficConfigID int64

	// FromDID is the number the call presents. Validated as owned-and-active
	// before it gets here — a carrier checks that the calling number belongs
	// to the account the call arrives on.
	FromDID string

	// ToNumber is who to ring.
	ToNumber string

	// TrunkHost is the carrier's SIP host, taken from the outbound line's trunk.
	TrunkHost string

	// TrunkPort is the carrier's SIP port. Zero or 5060 leaves it out of the
	// URI, so the common case reads exactly as it did before.
	TrunkPort int

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

		// -------------------------------------------------------------------
		// AND THE SAME NEXT HOP FOR EVERY LEG THIS CALL BRIDGES TO.
		//
		// sip_route_uri above applies to THIS leg — the one to the carrier.
		// When the customer answers, the controller bridges this channel to
		// LiveKit (or later to a human), and that new leg needs the same next
		// hop. On an inbound call the dialplan's `export nolocal:` provides
		// it; an originate runs no dialplan, so nothing did.
		//
		// export_vars copies the named variables onto every leg bridged from
		// this channel. Without it the LiveKit leg has no route, FreeSWITCH
		// tries to resolve the placeholder host itself, and fails locally:
		//
		//   New Channel sofia/internal/<room>@livekit.invalid
		//   entering state [terminated][503]   ← no INVITE ever leaves the box
		//
		// Observed in beta on the first outbound call: the customer answered
		// and was hung up on ~200ms later, every time.
		// -------------------------------------------------------------------
		"export_vars=sip_route_uri",

		// -------------------------------------------------------------------
		// AND THE DIALPLAN A TRANSFER LANDS IN.
		//
		// When the agent asks for a human, livekit-sip sends a REFER and
		// FreeSWITCH transfers THIS channel to `handoff` — in the channel's
		// own context. An inbound call has one (`public`, from the sofia
		// profile), where the catch-all hands it back to the controller. An
		// originate never entered a context, so FreeSWITCH falls back to
		// `default`, which has no dialplan:
		//
		//   Transfer sofia/internal/<customer>@<trunk> to XML[handoff@default]
		//   Hangup ... [CS_ROUTING] [NORMAL_TEMPORARY_FAILURE]
		//
		// Observed in beta on the first outbound transfer: the agent said it
		// was connecting the customer, and the customer was hung up on.
		// -------------------------------------------------------------------
		"force_transfer_context=" + TransferContext,

		"origination_uuid=" + p.CallUUID,
		"origination_caller_id_number=" + p.FromDID,

		// Marks the socket that arrives later as belonging to an outbound
		// call. Without it the controller would treat the answered channel as
		// an inbound one and try to resolve the dialled number as a DID we
		// own — which it is not.
		"intron_direction=outbound",
		"intron_tenant=" + p.TenantID,
		"intron_traffic_config=" + strconv.FormatInt(p.TrafficConfigID, 10),

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

	// The port goes in the Request-URI, not in sip_route_uri. The next hop is
	// always Kamailio on its own port; the Request-URI is what Kamailio relays
	// to, so that is where the carrier's port has to be. Dropping it sends
	// the INVITE to 5060 on the carrier's host, which a carrier listening
	// elsewhere never sees — silently, because nothing refuses it.
	host := p.TrunkHost
	if p.TrunkPort != 0 && p.TrunkPort != 5060 {
		host = fmt.Sprintf("%s:%d", p.TrunkHost, p.TrunkPort)
	}

	// THE APPLICATION IS QUOTED, AND IT HAS TO BE.
	//
	// `originate` splits its arguments on spaces, so an unquoted
	// `&socket(host:port async full)` reaches FreeSWITCH as three arguments:
	// the application gets `host:port` alone, and `async` and `full)` are read
	// as the dialplan and context. The socket then runs SYNCHRONOUS:
	//
	//   EXECUTE ... socket(esl.beta.intron.internal:8022)   ← no async, no full
	//
	// On a synchronous socket `execute bridge` replies only when the bridge
	// ENDS. The controller timed out waiting, called the bridge failed, and
	// sent a hangup — which queued behind the bridge for the whole
	// conversation and then hung the customer up the moment a transfer ended
	// the bridge. Every outbound call had this; the transfer was only where it
	// became audible. The inbound dialplan's socket was never affected: an XML
	// action's data is one argument, spaces and all.
	return fmt.Sprintf("{%s}sofia/internal/sip:%s@%s '&socket(%s async full)'",
		strings.Join(clean, ","),
		p.ToNumber, host, p.CallbackTarget), nil
}
