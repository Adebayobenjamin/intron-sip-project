package route

import (
	"strings"
	"testing"
)

func validOriginate() OriginateParams {
	return OriginateParams{
		CallUUID:       "fcc32595-715d-4f33-8557-84cdd2f43d22",
		TenantID:       "t_test",
		PlanID:         "p_ai",
		FromDID:        "+13343453663",
		ToNumber:       "+2349030622835",
		TrunkHost:      "intron-beta.pstn.twilio.com",
		CallbackTarget: "esl.beta.intron.internal:8022",
		EdgeSIPRoute:   "sip:10.70.0.10:5060",
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
