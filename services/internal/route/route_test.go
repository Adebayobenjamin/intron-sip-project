package route

import (
	"errors"
	"strings"
	"testing"
)

func params() Params {
	return Params{
		CallUUID: "c0ffee00-0000-4000-8000-000000000001",
		TenantID: "acme",
		CallerID: "+2348031234567",
		DID:      "+2348000000100",
	}
}

func TestLiveKit_RoomIsTheCallUUID(t *testing.T) {
	dial, err := LiveKit(params())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dial, "sip:c0ffee00-0000-4000-8000-000000000001@livekit.invalid") {
		t.Errorf("room is not the call uuid in the user part:\n%s", dial)
	}
	if !strings.Contains(dial, "sip_h_X-Intron-Egress="+EgressLiveKit) {
		t.Errorf("no livekit egress header; Kamailio would send this to a carrier:\n%s", dial)
	}
}

func TestLiveKit_RefusesWithoutAUUID(t *testing.T) {
	p := params()
	p.CallUUID = ""
	if _, err := LiveKit(p); err == nil {
		t.Fatal("built an AI leg with no room name")
	}
}

// THE PLATFORM-TRUNK HANDOFF. An E.164 target is a new call through the CALL'S
// carrier, so the host comes from the target's TrunkHost — which the store
// fills from the call's trunk — not from anything the tenant owns.
func TestHandoff_E164LeavesByTheCallsTrunk(t *testing.T) {
	dial, err := Handoff(params(), []HandoffTarget{
		{ID: 1, Kind: KindE164, Destination: "+2348099999999", TrunkHost: "x.pstn.twilio.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dial, "sip:+2348099999999@x.pstn.twilio.com") {
		t.Errorf("E.164 target not dialled through the trunk:\n%s", dial)
	}
	if !strings.Contains(dial, "sip_h_X-Intron-Egress="+EgressCarrier) {
		t.Errorf("no carrier egress header:\n%s", dial)
	}
}

// A carrier on another port is only reached if the port is in the
// Request-URI — Kamailio relays to what the URI names.
func TestHandoff_E164CarriesANonStandardPort(t *testing.T) {
	dial, err := Handoff(params(), []HandoffTarget{
		{ID: 1, Kind: KindE164, Destination: "+2348099999999", TrunkHost: "sbc.acme", TrunkPort: 5080},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dial, "@sbc.acme:5080") {
		t.Errorf("port dropped from the Request-URI:\n%s", dial)
	}

	dial, _ = Handoff(params(), []HandoffTarget{
		{ID: 1, Kind: KindE164, Destination: "+2348099999999", TrunkHost: "sbc.acme", TrunkPort: 5060},
	})
	if strings.Contains(dial, ":5060") {
		t.Errorf("the default port should be left out, as for every other leg:\n%s", dial)
	}
}

// An E.164 target with no trunk is skipped, not dialled at Kamailio itself.
// With nothing else to try, the handoff fails as having no target.
func TestHandoff_E164WithoutATrunkIsSkipped(t *testing.T) {
	_, err := Handoff(params(), []HandoffTarget{
		{ID: 1, Kind: KindE164, Destination: "+2348099999999"},
	})
	if !errors.Is(err, ErrNoHandoffTarget) {
		t.Fatalf("err = %v, want ErrNoHandoffTarget", err)
	}
}

// Failover order is the operator's priority order, and a bad target does not
// cost the good ones.
func TestHandoff_KeepsOrderAndSkipsTheUndiallable(t *testing.T) {
	dial, err := Handoff(params(), []HandoffTarget{
		{ID: 1, Kind: KindSIPURI, Destination: "queue@sbc.acme"},
		{ID: 2, Kind: KindE164, Destination: "+2348099999999"}, // no trunk
		{ID: 3, Kind: KindE164, Destination: "+2348088888888", TrunkHost: "x.pstn.twilio.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	legs := strings.Split(dial, "|")
	if len(legs) != 2 {
		t.Fatalf("want 2 legs, got %d:\n%s", len(legs), dial)
	}
	if !strings.Contains(legs[0], "sip:queue@sbc.acme") || !strings.Contains(legs[1], "+2348088888888") {
		t.Errorf("legs out of order:\n%s", dial)
	}
}
