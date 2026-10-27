package ctrl

import (
	"errors"
	"strings"
	"testing"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/route"
)

// answeredOutbound is the socket FreeSWITCH opens when the far end of an
// originate picks up: the variables the originate set, and the customer's
// number as the destination.
func answeredOutbound() channelData {
	ch := defaultChannel()
	ch.destination = "+2349030622835"
	ch.vars = map[string]string{
		"intron_direction":      "outbound",
		"intron_tenant":         "t_acme",
		"intron_traffic_config": "21",
	}
	return ch
}

// The slot the control plane claimed when the call was queued.
func outboundSlot(mode string) *fakeLive {
	return &fakeLive{call: calls.Live{
		TenantID:        "t_acme",
		TrafficConfigID: 21,
		TrunkID:         9,
		Direction:       calls.DirectionOutbound,
		RecordingMode:   mode,
	}}
}

// What the answered customer is connected to comes from the line; the trunk
// and the recording policy come from the slot claimed when the call started.
// The line here says "none" and the snapshot says "full": the snapshot wins,
// because the call started under it.
func TestOutbound_LineDecidesTheDestinationSnapshotDecidesRecording(t *testing.T) {
	routes := handoffRoutes()
	routes.line = route.Line{ID: 21, TenantID: "t_acme", TrunkID: 9,
		Destination: "handoff", RecordingMode: "none"}

	ledger := &fakeLedger{}
	ch := answeredOutbound()
	addr := startListenerLive(t, &stubResolver{err: errors.New("outbound must not be resolved")},
		ledger, routes, &fakeProbe{}, outboundSlot("full"))
	fs := dialFakeFS(t, addr, ch)
	cmds := fs.serveCallThenHangup(ch.uuid, "NORMAL_CLEARING", "recv_bye")
	assertBridged(t, cmds)

	// Already answered by definition — the socket opens on answer.
	if i := commandIndex(cmds, "execute-app-name: answer"); i >= 0 {
		t.Error("an outbound channel was answered again")
	}
	if i := commandIndex(cmds, "execute-app-name: record_session"); i < 0 {
		t.Error("the snapshot said full and nothing was recorded")
	}

	q := routes.handoffQueries()
	want := handoffQuery{tenant: "t_acme", trafficConfigID: 21, trunkID: 9}
	if len(q) != 1 || q[0] != want {
		t.Errorf("handoff asked for %+v, want [%+v]", q, want)
	}

	if n := len(ledger.claimed()); n != 0 {
		t.Errorf("claimed %d slot(s); the control plane already claimed this call's", n)
	}
}

// If the slot cannot be read, the line's own settings are the fallback.
func TestOutbound_FallsBackToTheLineWhenTheSlotIsUnreadable(t *testing.T) {
	routes := &fakeRoutes{line: route.Line{ID: 21, TenantID: "t_acme", TrunkID: 9,
		Destination: "ai", RecordingMode: "ai_only"}}

	ch := answeredOutbound()
	addr := startListenerLive(t, &stubResolver{}, &fakeLedger{}, routes, &fakeProbe{},
		&fakeLive{err: errors.New("connection refused")})
	fs := dialFakeFS(t, addr, ch)
	cmds := fs.serveCallThenHangup(ch.uuid, "NORMAL_CLEARING", "recv_bye")
	assertBridged(t, cmds)

	if i := commandIndex(cmds, "execute-app-name: record_session"); i < 0 {
		t.Error("the line says ai_only on an AI call and nothing was recorded")
	}
	if b, _ := findCommand(cmds, "execute-app-name: bridge"); !strings.Contains(b, route.EgressLiveKit) {
		t.Errorf("the line says ai and the call did not go to LiveKit: %q", b)
	}
}

// With no line readable at all: the AI agent, and nothing recorded — recording
// is not something to guess at on a call that is already connected.
func TestOutbound_DefaultsAreAIAndUnrecorded(t *testing.T) {
	routes := &fakeRoutes{lineErr: errors.New("not found")}

	ch := answeredOutbound()
	addr := startListenerLive(t, &stubResolver{}, &fakeLedger{}, routes, &fakeProbe{},
		&fakeLive{err: errors.New("connection refused")})
	fs := dialFakeFS(t, addr, ch)
	cmds := fs.serveCallThenHangup(ch.uuid, "NORMAL_CLEARING", "recv_bye")
	assertBridged(t, cmds)

	if strings.Contains(strings.Join(cmds, "\n"), "record_session") {
		t.Error("recorded a call whose policy could not be read")
	}
}

// ---------------------------------------------------------------------------
// The decision table
// ---------------------------------------------------------------------------

func TestRecordingActionFor(t *testing.T) {
	ai, handoff := admission.DestinationAI, admission.DestinationHandoff
	none, aiOnly, full := admission.RecordingNone, admission.RecordingAIOnly, admission.RecordingFull

	cases := []struct {
		mode        admission.RecordingMode
		dest        admission.Destination
		transferred bool
		want        recordingAction
	}{
		{none, ai, false, recordingLeave},
		{none, handoff, true, recordingLeave},
		{aiOnly, ai, false, recordingStart},
		{aiOnly, handoff, false, recordingLeave}, // human from the start
		{aiOnly, handoff, true, recordingStop},   // the human is taking over
		{full, ai, false, recordingStart},
		{full, handoff, false, recordingStart},
		{full, handoff, true, recordingLeave}, // already running; must carry on
		{"", ai, false, recordingLeave},       // unset is none
	}

	for _, c := range cases {
		got := recordingActionFor(c.mode, c.dest, c.transferred)
		if got != c.want {
			t.Errorf("mode=%q dest=%q transferred=%v: got %s, want %s",
				c.mode, c.dest, c.transferred, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Transfers of outbound calls
// ---------------------------------------------------------------------------

// THE OUTBOUND HANDOFF BUG. The leg that comes back after a REFER still carries
// intron_direction=outbound, and the direction was checked first — so it was
// treated as a fresh outbound call and bridged straight back to the AI agent
// that had just asked for a human.
func TestOutbound_TransferredLegGoesToAHumanNotBackToTheAI(t *testing.T) {
	ch := answeredOutbound()
	ch.destination = TransferDestination

	live := outboundSlot("full")
	live.call.CallUUID = ch.uuid

	routes := handoffRoutes()
	routes.lineErr = errors.New("the line must not be consulted for a transfer")

	addr := startListenerLive(t, &stubResolver{err: errors.New("must not be resolved")},
		&fakeLedger{}, routes, &fakeProbe{}, live)
	fs := dialFakeFS(t, addr, ch)
	cmds := fs.serveCallThenHangup(ch.uuid, "NORMAL_CLEARING", "recv_bye")
	assertBridged(t, cmds)

	bridge, _ := findCommand(cmds, "execute-app-name: bridge")
	if strings.Contains(bridge, route.EgressLiveKit) {
		t.Fatalf("the transferred customer was sent back to the AI agent:\n%s", bridge)
	}
	if !strings.Contains(bridge, "sbc.client.example") {
		t.Errorf("not bridged to the handoff target:\n%s", bridge)
	}

	// The call's own trunk, from the slot the control plane claimed.
	q := routes.handoffQueries()
	want := handoffQuery{tenant: "t_acme", trafficConfigID: 21, trunkID: 9}
	if len(q) != 1 || q[0] != want {
		t.Errorf("handoff asked for %+v, want [%+v]", q, want)
	}

	// Already answered — an originated channel — so no second answer.
	if i := commandIndex(cmds, "execute-app-name: answer"); i >= 0 {
		t.Error("the transferred outbound leg was answered again")
	}
}

// The ORIGINAL outbound leg, whose bridge ends because of the transfer, must
// wait for the call to really end — not write the record and free the slot
// while the customer is talking to the human.
func TestOutbound_OriginalLegHoldsTheSlotThroughATransfer(t *testing.T) {
	ledger := &fakeLedger{}

	// Alive on the first check, gone on the second: transferred, then ended.
	probe := &fakeProbe{alive: 1}

	routes := &fakeRoutes{line: route.Line{ID: 21, TenantID: "t_acme", TrunkID: 9,
		Destination: "ai", RecordingMode: "none"}}

	addr := startListenerLive(t, &stubResolver{}, ledger, routes, probe, outboundSlot("none"))
	fs := dialFakeFS(t, addr, answeredOutbound())

	// Answers every command and never sends a hangup event: the bridge
	// returns with the channel still up, which is what a transfer looks like.
	fs.serveUntilClose()

	rec := ledger.waitForRecord(t)
	if rec.Outcome != outcomeTransferred {
		t.Errorf("Outcome = %q, want %q", rec.Outcome, outcomeTransferred)
	}
	if n := probe.probes(); n < 2 {
		t.Errorf("the original leg finalised without waiting for the call to end: %d probes", n)
	}
}
