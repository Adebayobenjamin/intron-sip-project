package ctrl

import (
	"errors"
	"strings"
	"testing"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/route"
)

// playCall runs a whole call and returns every command the controller issued.
func playCall(t *testing.T, r Resolver, l Ledger, routes Routes) []string {
	t.Helper()

	fs := dialFakeFS(t, startListenerWithRoutes(t, r, l, routes), defaultChannel())
	return fs.serveUntilClose()
}

func aiPlan() admission.Verdict {
	v := admitted()
	v.Destination = admission.DestinationAI
	return v
}

func handoffPlan() admission.Verdict {
	v := admitted()
	v.Destination = admission.DestinationHandoff
	v.Recording = false
	return v
}

// ---------------------------------------------------------------------------
// Answering
// ---------------------------------------------------------------------------

func TestConnect_AnswersThenBridges(t *testing.T) {
	cmds := playCall(t, &stubResolver{verdict: aiPlan()}, &fakeLedger{}, &fakeRoutes{})

	answer := commandIndex(cmds, "execute-app-name: answer")
	bridge := commandIndex(cmds, "execute-app-name: bridge")

	if answer < 0 {
		t.Fatalf("the call was never answered: %v", cmds)
	}
	if bridge < 0 {
		t.Fatalf("the call was never bridged: %v", cmds)
	}
	if answer > bridge {
		t.Error("the call was bridged before it was answered")
	}
}

// A plan that cannot be routed must be refused BEFORE the answer. Answering
// first starts carrier billing on a call that is about to fail, and makes the
// failure look to the caller like a connection that died.
func TestConnect_UnroutablePlanIsRefusedBeforeAnswer(t *testing.T) {
	routes := &fakeRoutes{agentErr: route.ErrNoAgentConfig}
	ledger := &fakeLedger{}

	cmds := playCall(t, &stubResolver{verdict: aiPlan()}, ledger, routes)

	if i := commandIndex(cmds, "execute-app-name: answer"); i >= 0 {
		t.Error("an unroutable call was answered")
	}

	hangup, ok := findCommand(cmds, "call-command: hangup")
	if !ok {
		t.Fatalf("no hangup: %v", cmds)
	}
	// Our misconfiguration, not the caller's problem: the number is valid and
	// will work once someone finishes setting it up.
	if !strings.Contains(hangup, causeTemporary) {
		t.Errorf("want %s, got %q", causeTemporary, hangup)
	}

	rec := ledger.waitForRecord(t)
	if rec.Outcome != "unroutable" {
		t.Errorf("Outcome = %q, want unroutable", rec.Outcome)
	}
}

// A tenant with no enabled handoff target cannot take a call routed to a
// human. Same reasoning: refuse before answering.
func TestConnect_NoHandoffTargetIsRefusedBeforeAnswer(t *testing.T) {
	cmds := playCall(t, &stubResolver{verdict: handoffPlan()}, &fakeLedger{},
		&fakeRoutes{targets: nil})

	if i := commandIndex(cmds, "execute-app-name: answer"); i >= 0 {
		t.Error("a call with nowhere to go was answered")
	}
	if _, ok := findCommand(cmds, causeTemporary); !ok {
		t.Errorf("want a temporary failure: %v", cmds)
	}
}

// ---------------------------------------------------------------------------
// The LiveKit leg
// ---------------------------------------------------------------------------

func TestBridge_LiveKitDialString(t *testing.T) {
	cmds := playCall(t, &stubResolver{verdict: aiPlan()}, &fakeLedger{}, &fakeRoutes{})

	bridge, ok := findCommand(cmds, "execute-app-name: bridge")
	if !ok {
		t.Fatalf("no bridge: %v", cmds)
	}

	// The next hop is NOT set here. The controller never talks to Kamailio;
	// the dialplan exports sip_route_uri onto the outbound leg from
	// edge_private_ips, so the address lives in exactly one place.
	if strings.Contains(bridge, "sip_route_uri") {
		t.Errorf("the controller should not be setting the next hop: %q", bridge)
	}

	// The header is what Kamailio branches on. Inferring direction from the
	// shape of the URI breaks the first time a handoff target is a bare E.164.
	if !strings.Contains(bridge, "sip_h_X-Intron-Egress="+route.EgressLiveKit) {
		t.Errorf("missing the livekit egress header: %q", bridge)
	}

	// The room goes in the user part, which is what livekit-sip matches a
	// dispatch rule against. With no template it defaults to the channel UUID.
	if !strings.Contains(bridge, defaultChannel().uuid+"@") {
		t.Errorf("the room is not in the user part: %q", bridge)
	}
}

// A room template lets the LiveKit side recognise something in the name.
func TestBridge_RoomTemplateIsRendered(t *testing.T) {
	ledger := &fakeLedger{}
	routes := &fakeRoutes{agent: route.AgentConfig{
		RoomTemplate:  "intron-{tenant_id}-{call_uuid}",
		AgentIdentity: "agent",
	}}

	cmds := playCall(t, &stubResolver{verdict: aiPlan()}, ledger, routes)

	want := "intron-t_acme-" + defaultChannel().uuid
	bridge, _ := findCommand(cmds, "execute-app-name: bridge")
	if !strings.Contains(bridge, want) {
		t.Errorf("want room %q in %q", want, bridge)
	}

	// And it reaches the record, so a call can be found from the LiveKit side.
	rec := ledger.waitForRecord(t)
	if rec.LiveKitRoom != want {
		t.Errorf("LiveKitRoom = %q, want %q", rec.LiveKitRoom, want)
	}
}

// ---------------------------------------------------------------------------
// The handoff leg
// ---------------------------------------------------------------------------

func TestBridge_HandoffUsesFailoverOrder(t *testing.T) {
	routes := &fakeRoutes{targets: []route.HandoffTarget{
		{ID: 1, Kind: route.KindSIPURI, Destination: "sip:queue@sbc.client.example"},
		{ID: 2, Kind: route.KindE164, Destination: "+442071234567", TrunkHost: "trunk.example"},
	}}

	cmds := playCall(t, &stubResolver{verdict: handoffPlan()}, &fakeLedger{}, routes)

	bridge, ok := findCommand(cmds, "execute-app-name: bridge")
	if !ok {
		t.Fatalf("no bridge: %v", cmds)
	}

	// `|` is FreeSWITCH's failover separator: try the next leg when one fails.
	if !strings.Contains(bridge, "|") {
		t.Errorf("want two legs separated by |, got %q", bridge)
	}
	if !strings.Contains(bridge, "sip:queue@sbc.client.example") {
		t.Errorf("first target missing: %q", bridge)
	}
	// An E.164 target leaves by the tenant's trunk.
	if !strings.Contains(bridge, "+442071234567@trunk.example") {
		t.Errorf("second target missing its trunk: %q", bridge)
	}
	if !strings.Contains(bridge, "sip_h_X-Intron-Egress="+route.EgressCarrier) {
		t.Errorf("missing the carrier egress header: %q", bridge)
	}
}

// The agent needs to see who is calling. A handoff presenting the DID makes
// every transferred call look identical.
func TestBridge_HandoffPresentsTheOriginalCaller(t *testing.T) {
	routes := &fakeRoutes{targets: []route.HandoffTarget{
		{ID: 1, Kind: route.KindSIPURI, Destination: "sip:queue@sbc.client.example"},
	}}

	cmds := playCall(t, &stubResolver{verdict: handoffPlan()}, &fakeLedger{}, routes)

	bridge, _ := findCommand(cmds, "execute-app-name: bridge")
	if !strings.Contains(bridge, "origination_caller_id_number="+defaultChannel().caller) {
		t.Errorf("the caller's number is not presented: %q", bridge)
	}
}

// A target that cannot be dialled is skipped, not fatal. A misconfigured
// third target should not cost a call the first two could have taken.
func TestBridge_SkipsUndiallableTargets(t *testing.T) {
	routes := &fakeRoutes{targets: []route.HandoffTarget{
		// E.164 with no trunk to leave by.
		{ID: 1, Kind: route.KindE164, Destination: "+442071234567"},
		{ID: 2, Kind: route.KindSIPURI, Destination: "sip:queue@sbc.client.example"},
	}}

	cmds := playCall(t, &stubResolver{verdict: handoffPlan()}, &fakeLedger{}, routes)

	bridge, ok := findCommand(cmds, "execute-app-name: bridge")
	if !ok {
		t.Fatalf("the call should still bridge to the usable target: %v", cmds)
	}
	if strings.Contains(bridge, "|") {
		t.Errorf("the undiallable target should have been dropped: %q", bridge)
	}
	if !strings.Contains(bridge, "sip:queue@sbc.client.example") {
		t.Errorf("the usable target is missing: %q", bridge)
	}
}

// ---------------------------------------------------------------------------
// Recording
// ---------------------------------------------------------------------------

// Started after answer — there is no media before it — and before the bridge,
// so the whole conversation is captured rather than everything after the far
// end picked up.
func TestRecording_StartsBetweenAnswerAndBridge(t *testing.T) {
	v := aiPlan()
	v.Recording = true

	cmds := playCall(t, &stubResolver{verdict: v}, &fakeLedger{}, &fakeRoutes{})

	answer := commandIndex(cmds, "execute-app-name: answer")
	record := commandIndex(cmds, "execute-app-name: record_session")
	bridge := commandIndex(cmds, "execute-app-name: bridge")

	if record < 0 {
		t.Fatalf("recording never started: %v", cmds)
	}
	if !(answer < record && record < bridge) {
		t.Errorf("want answer < record < bridge, got %d %d %d", answer, record, bridge)
	}
}

// Stereo puts each leg on its own channel. A mixed-down mono file cannot be
// separated back into who said what, which is most of what a recording is for.
func TestRecording_IsStereo(t *testing.T) {
	v := aiPlan()
	v.Recording = true

	cmds := playCall(t, &stubResolver{verdict: v}, &fakeLedger{}, &fakeRoutes{})

	set, ok := findCommand(cmds, "RECORD_STEREO")
	if !ok {
		t.Fatalf("stereo was not set: %v", cmds)
	}
	if !strings.Contains(set, "RECORD_STEREO=true") {
		t.Errorf("want RECORD_STEREO=true, got %q", set)
	}
}

// The path is the contract between FreeSWITCH writing the file, the uploader
// shipping it, and the record pointing at it. Fixed now, while nothing reads
// it, because changing it later means finding all three.
func TestRecording_PathShapeAndRecord(t *testing.T) {
	v := aiPlan()
	v.Recording = true
	ledger := &fakeLedger{}

	cmds := playCall(t, &stubResolver{verdict: v}, ledger, &fakeRoutes{})

	want := "/mnt/recordings/t_acme__" + defaultChannel().uuid + ".wav"

	rs, _ := findCommand(cmds, "record_session")
	if !strings.Contains(rs, want) {
		t.Errorf("want path %q in %q", want, rs)
	}

	rec := ledger.waitForRecord(t)
	if rec.RecordingURL != want {
		t.Errorf("RecordingURL = %q, want %q", rec.RecordingURL, want)
	}
}

func TestRecording_SkippedWhenThePlanSaysSo(t *testing.T) {
	v := aiPlan()
	v.Recording = false

	cmds := playCall(t, &stubResolver{verdict: v}, &fakeLedger{}, &fakeRoutes{})

	if i := commandIndex(cmds, "record_session"); i >= 0 {
		t.Error("recording started for a plan that does not ask for it")
	}
}

// ---------------------------------------------------------------------------
// The record
// ---------------------------------------------------------------------------

func TestConnect_BridgedCallIsRecordedAsSuch(t *testing.T) {
	ledger := &fakeLedger{}
	playCallWithHangup(t, &stubResolver{verdict: aiPlan()}, ledger, &fakeRoutes{},
		"NORMAL_CLEARING", "send_bye")

	rec := ledger.waitForRecord(t)

	if rec.Outcome != "bridged" {
		t.Errorf("Outcome = %q, want bridged", rec.Outcome)
	}
	if rec.AnswerAt.IsZero() {
		t.Error("AnswerAt must be set for a call that answered")
	}
	// Read off the hangup event, not inferred from the bridge returning.
	if rec.HangupBy != calls.HangupByCallee {
		t.Errorf("HangupBy = %q, want %q", rec.HangupBy, calls.HangupByCallee)
	}
}

// A bridged call whose hangup event never arrives leaves HangupBy unset rather
// than attributing it to someone. The platform did not hang up, and nothing
// says who did.
func TestConnect_BridgedCallWithNoEventLeavesHangupByUnset(t *testing.T) {
	ledger := &fakeLedger{}
	playCall(t, &stubResolver{verdict: aiPlan()}, ledger, &fakeRoutes{})

	rec := ledger.waitForRecord(t)
	if rec.HangupBy != "" {
		t.Errorf("HangupBy = %q, want empty — nothing reported who hung up", rec.HangupBy)
	}
}

// A ledger write failing must not drop a call that is up. The caller is
// connected; the record is reconstructed at hangup.
func TestConnect_LedgerWriteFailureDoesNotDropTheCall(t *testing.T) {
	ledger := &fakeLedger{stateErr: errors.New("connection refused")}

	cmds := playCall(t, &stubResolver{verdict: aiPlan()}, ledger, &fakeRoutes{})

	if i := commandIndex(cmds, "execute-app-name: bridge"); i < 0 {
		t.Errorf("the call was dropped over a state write: %v", cmds)
	}
}
