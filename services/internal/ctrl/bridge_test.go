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
	v.RecordingMode = admission.RecordingNone
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

// A line that cannot be routed must be refused BEFORE the answer. Answering
// first starts carrier billing on a call that is about to fail, and makes the
// failure look to the caller like a connection that died.
func TestConnect_UnroutableLineIsRefusedBeforeAnswer(t *testing.T) {
	routes := &fakeRoutes{targetsErr: errors.New("connection refused")}
	ledger := &fakeLedger{}

	cmds := playCall(t, &stubResolver{verdict: handoffPlan()}, ledger, routes)

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

	// The room — the channel UUID — goes in the user part.
	if !strings.Contains(bridge, defaultChannel().uuid+"@") {
		t.Errorf("the room is not in the user part: %q", bridge)
	}
}

// The AI leg needs no per-line configuration: every call reaches the same
// agent, summoned by LiveKit's dispatch rule. So routing is never consulted,
// and the room the record names is the one in the dial string.
func TestBridge_AILegNeedsNoRoutingLookup(t *testing.T) {
	ledger := &fakeLedger{}
	routes := &fakeRoutes{targetsErr: errors.New("must not be consulted for the AI leg")}

	cmds := playCall(t, &stubResolver{verdict: aiPlan()}, ledger, routes)
	if i := commandIndex(cmds, "execute-app-name: bridge"); i < 0 {
		t.Fatalf("the AI leg was not bridged: %v", cmds)
	}

	rec := ledger.waitForRecord(t)
	if rec.LiveKitRoom != defaultChannel().uuid {
		t.Errorf("LiveKitRoom = %q, want the call uuid", rec.LiveKitRoom)
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
	// An E.164 target leaves by the call's trunk.
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
	v.RecordingMode = admission.RecordingAIOnly

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
	v.RecordingMode = admission.RecordingFull

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
	v.RecordingMode = admission.RecordingAIOnly
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

func TestRecording_SkippedWhenTheLineSaysSo(t *testing.T) {
	v := aiPlan()
	v.RecordingMode = admission.RecordingNone

	cmds := playCall(t, &stubResolver{verdict: v}, &fakeLedger{}, &fakeRoutes{})

	if i := commandIndex(cmds, "record_session"); i >= 0 {
		t.Error("recording started for a line that does not ask for it")
	}
}

// ai_only on a line that goes straight to a human has no AI half to record.
// Starting a recording here would record exactly the conversation the policy
// exists to leave alone.
func TestRecording_AIOnlyLineGoingStraightToAHumanRecordsNothing(t *testing.T) {
	v := handoffPlan()
	v.RecordingMode = admission.RecordingAIOnly

	cmds := playCall(t, &stubResolver{verdict: v}, &fakeLedger{}, handoffRoutes())
	assertBridged(t, cmds)

	if i := commandIndex(cmds, "record_session"); i >= 0 {
		t.Error("an ai_only line recorded a call that never reached the AI")
	}
}

// full records a human from the start, too.
func TestRecording_FullRecordsAStraightHandoff(t *testing.T) {
	v := handoffPlan()
	v.RecordingMode = admission.RecordingFull

	cmds := playCall(t, &stubResolver{verdict: v}, &fakeLedger{}, handoffRoutes())
	if i := commandIndex(cmds, "execute-app-name: record_session"); i < 0 {
		t.Errorf("a full-recording line did not record: %v", cmds)
	}
}

// The mode the call ran under reaches the record, so a recording that is or is
// not there can be explained later.
func TestRecording_ModeReachesTheRecord(t *testing.T) {
	v := aiPlan()
	v.RecordingMode = admission.RecordingFull
	ledger := &fakeLedger{}

	playCall(t, &stubResolver{verdict: v}, ledger, &fakeRoutes{})

	if rec := ledger.waitForRecord(t); rec.RecordingMode != "full" {
		t.Errorf("RecordingMode = %q, want full", rec.RecordingMode)
	}
}

// ---------------------------------------------------------------------------
// The handoff trunk
// ---------------------------------------------------------------------------

// THE PLATFORM-TRUNK BUG. An E.164 target must leave by the trunk the call is
// on, which admission found from the number. The old lookup joined "the
// tenant's default trunk", which a tenant on the platform trunk does not have.
func TestBridge_HandoffAsksForTheCallsTrunk(t *testing.T) {
	routes := handoffRoutes()
	playCall(t, &stubResolver{verdict: handoffPlan()}, &fakeLedger{}, routes)

	q := routes.handoffQueries()
	if len(q) != 1 {
		t.Fatalf("HandoffTargets called %d times, want 1", len(q))
	}
	want := handoffQuery{tenant: "t_acme", trafficConfigID: 21, trunkID: 9}
	if q[0] != want {
		t.Errorf("asked for %+v, want %+v — the line for overrides, the call's trunk for E.164", q[0], want)
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
