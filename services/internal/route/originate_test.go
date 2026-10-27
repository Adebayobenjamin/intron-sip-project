package route

import (
	"strings"
	"testing"
)

func validOriginate() OriginateParams {
	return OriginateParams{
		CallUUID:        "fcc32595-715d-4f33-8557-84cdd2f43d22",
		TenantID:        "t_test",
		TrafficConfigID: 42,
		FromDID:         "+13343453663",
		ToNumber:        "+2349030622835",
		TrunkHost:       "intron-beta.pstn.twilio.com",
		CallbackTarget:  "esl.beta.intron.internal:8022",
		EdgeSIPRoute:    "sip:10.70.0.10:5060",
	}
}

// THE TEST FOR THE BUG THAT REACHED BETA.
//
// Without sip_route_uri, FreeSWITCH resolves the carrier hostname and sends
// the INVITE straight out of the private subnet. Every layer reported success
// — bgapi returned a job id, the request row read `placed` — and nothing ever
// arrived at Kamailio. An outbound call that silently does not happen.
func TestOriginate_SendsTheCallViaTheEdgeNotStraightToTheCarrier(t *testing.T) {
	dial, err := Originate(validOriginate())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(dial, "sip_route_uri=sip:10.70.0.10:5060") {
		t.Fatalf("no sip_route_uri: the INVITE would go straight at the carrier "+
			"and never reach the edge.\ndial: %s", dial)
	}

	// The Request-URI must STILL name the carrier. The two are different
	// things: the URI says who the call is for, sip_route_uri says who to hand
	// the packet to. Collapsing them into one breaks the other direction.
	if !strings.Contains(dial, "sip:+2349030622835@intron-beta.pstn.twilio.com") {
		t.Errorf("the Request-URI no longer names the carrier destination:\n%s", dial)
	}
}

// Refused rather than defaulted. A dial string without it is accepted by
// FreeSWITCH and placed, so the failure has to happen here or not at all.
func TestOriginate_RefusesWithoutTheEdgeRoute(t *testing.T) {
	p := validOriginate()
	p.EdgeSIPRoute = ""

	if _, err := Originate(p); err == nil {
		t.Fatal("built a dial string with no next hop; FreeSWITCH would accept " +
			"it and the call would never reach the edge")
	}
}

// The edge address must not be guessed from anything else in the call.
func TestOriginate_UsesTheConfiguredEdgeVerbatim(t *testing.T) {
	p := validOriginate()
	p.EdgeSIPRoute = "sip:10.99.0.1:5080"

	dial, err := Originate(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dial, "sip_route_uri=sip:10.99.0.1:5080") {
		t.Errorf("edge route not carried through verbatim:\n%s", dial)
	}
}

// The answered socket finds its line from this variable. Without it the
// controller falls back to defaults — AI leg, unrecorded — whatever the line
// says.
func TestOriginate_CarriesTheLine(t *testing.T) {
	dial, err := Originate(validOriginate())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dial, "intron_traffic_config=42") {
		t.Errorf("the line id is not on the channel:\n%s", dial)
	}
}

// THE TEST FOR THE SECOND OUTBOUND BUG IN BETA.
//
// sip_route_uri routes the leg to the carrier. When the customer answers, the
// controller bridges this channel to LiveKit, and an originate runs no
// dialplan to export the route onto that new leg — so it had none, and failed
// inside FreeSWITCH with a 503 ~200ms after the customer picked up.
func TestOriginate_ExportsTheRouteToBridgedLegs(t *testing.T) {
	dial, err := Originate(validOriginate())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dial, "export_vars=sip_route_uri") {
		t.Fatalf("sip_route_uri is not exported: the leg bridged after answer "+
			"would have no next hop.\ndial: %s", dial)
	}
}

// THE TEST FOR THE THIRD OUTBOUND BUG IN BETA.
//
// A transfer re-enters the dialplan in the channel's own context. An originate
// has none, so FreeSWITCH used `default` — no dialplan there — and the
// customer was hung up on at the moment the agent handed them to a human.
func TestOriginate_TransfersLandInTheInboundDialplan(t *testing.T) {
	dial, err := Originate(validOriginate())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dial, "force_transfer_context="+TransferContext) {
		t.Fatalf("a transfer of this call would land in `default`, which has no "+
			"dialplan.\ndial: %s", dial)
	}
}

// THE TEST FOR THE FOURTH OUTBOUND BUG IN BETA.
//
// originate splits its arguments on spaces. Unquoted, `async full` fell off
// the socket application, the socket ran synchronous, the controller never
// got a reply to `bridge`, and its fallback hangup queued behind the bridge
// until a transfer released it — onto the customer.
func TestOriginate_SocketIsQuotedSoItRunsAsync(t *testing.T) {
	dial, err := Originate(validOriginate())
	if err != nil {
		t.Fatal(err)
	}
	want := "'&socket(esl.beta.intron.internal:8022 async full)'"
	if !strings.HasSuffix(dial, " "+want) {
		t.Fatalf("the socket application must be one quoted argument, ending the "+
			"command: want suffix %q\ndial: %s", want, dial)
	}
}
