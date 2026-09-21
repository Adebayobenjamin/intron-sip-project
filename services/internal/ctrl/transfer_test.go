package ctrl

import (
	"errors"
	"testing"
	"time"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
)

// playTransferredCall runs an admitted call whose bridge ends with NO hangup
// event — which is what a transfer looks like from inside the controller — and
// lets the probe decide whether the channel is still up.
func playTransferredCall(t *testing.T, ledger *fakeLedger, probe *fakeProbe) {
	t.Helper()

	addr := startListenerFull(t, &stubResolver{verdict: aiPlan()}, ledger, &fakeRoutes{}, probe)
	fs := dialFakeFS(t, addr, defaultChannel())

	// Answers every command and never sends a hangup event, so the bridge
	// returns and the watcher times out.
	fs.serveUntilClose()
}

// The whole point. A transferred call is still connected, so finalising at the
// transfer releases the concurrency slot on a live call — and a cap that frees
// up early permits more calls than it should.
func TestTransfer_SlotIsHeldWhileTheCallIsStillUp(t *testing.T) {
	ledger := &fakeLedger{}

	// Alive on the first check, gone on the second: transferred, then ended.
	probe := &fakeProbe{alive: 1}

	playTransferredCall(t, ledger, probe)

	rec := ledger.waitForRecord(t)
	if rec.Outcome != outcomeTransferred {
		t.Errorf("Outcome = %q, want %q", rec.Outcome, outcomeTransferred)
	}

	// The record must not be written until the channel is actually gone, which
	// means the probe was asked more than once.
	if n := probe.probes(); n < 2 {
		t.Errorf("the controller finalised without waiting: %d probes", n)
	}
}

// An ordinary call — one that really ended — must still finalise immediately.
// A transfer check that held every call open would leak a slot per call.
func TestTransfer_OrdinaryCallStillFinalisesAtOnce(t *testing.T) {
	ledger := &fakeLedger{}
	probe := &fakeProbe{alive: 0} // gone the moment we ask

	start := time.Now()
	playTransferredCall(t, ledger, probe)

	rec := ledger.waitForRecord(t)
	if rec.Outcome == outcomeTransferred {
		t.Error("a call that ended was treated as transferred")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("finalising took %s; it should not have waited", elapsed)
	}
}

// A hangup event means the call ended, full stop. Probing after one would be a
// pointless round trip on every single call.
func TestTransfer_NotCheckedWhenAHangupWasSeen(t *testing.T) {
	ledger := &fakeLedger{}
	probe := &fakeProbe{alive: 5}

	addr := startListenerFull(t, &stubResolver{verdict: aiPlan()}, ledger, &fakeRoutes{}, probe)
	fs := dialFakeFS(t, addr, defaultChannel())
	fs.serveCallThenHangup(defaultChannel().uuid, "NORMAL_CLEARING", "recv_bye")

	rec := ledger.waitForRecord(t)
	if rec.Outcome == outcomeTransferred {
		t.Error("a call with a hangup event was treated as transferred")
	}
	if n := probe.probes(); n != 0 {
		t.Errorf("the probe was asked %d times despite a hangup event", n)
	}
}

// Unable to tell is treated as ended. The conservative direction: the record is
// written now rather than held open on a call that may be long over, and the
// slot is released rather than leaked.
func TestTransfer_ProbeFailureTreatsTheCallAsEnded(t *testing.T) {
	ledger := &fakeLedger{}
	probe := &fakeProbe{err: errors.New("no FreeSWITCH node is connected")}

	playTransferredCall(t, ledger, probe)

	rec := ledger.waitForRecord(t)
	if rec.Outcome == outcomeTransferred {
		t.Error("an unanswerable probe was treated as a transfer")
	}
}

// Without a probe the controller must behave as it did before — wrong for
// transfers, but a service that starts.
func TestTransfer_NoProbeFallsBackToTheOldBehaviour(t *testing.T) {
	ledger := &fakeLedger{}

	l := New(Config{Addr: "127.0.0.1:0", HangupWait: 50 * time.Millisecond},
		&stubResolver{}, ledger, &fakeRoutes{}, nil, nil, quiet())

	if l.wasTransferred(t.Context(), quiet(), "any-uuid", false) {
		t.Error("a nil probe must not report a transfer")
	}
}

// The transfer is not an ending, so whatever the AI leg reported about how it
// finished must not be recorded as how the CALL finished. The final write owns
// those fields.
func TestTransfer_ClearsTheLegsTerminalFields(t *testing.T) {
	rec := calls.Record{
		HangupCause: causeTemporary,
		HangupBy:    calls.HangupByPlatform,
		SIPCode:     503,
		InviteAt:    time.Now().Add(-time.Minute),
	}

	l := New(Config{TransferPoll: 20 * time.Millisecond},
		&stubResolver{}, &fakeLedger{}, &fakeRoutes{}, &fakeProbe{alive: 0}, nil, quiet())

	l.watchTransferred(t.Context(), quiet(), &rec, "uuid")

	if rec.HangupCause != "" || rec.HangupBy != "" || rec.SIPCode != 0 {
		t.Errorf("the AI leg's ending was kept as the call's: %+v", rec)
	}
	if rec.Outcome != outcomeTransferred {
		t.Errorf("Outcome = %q", rec.Outcome)
	}
	if rec.EndAt.IsZero() {
		t.Error("EndAt must be set once the channel is gone")
	}
}

// The reserved Refer-To destination is what stops a transferred call being
// resolved as a phone number.
func TestIsTransferDestination(t *testing.T) {
	for _, s := range []string{"handoff", "HANDOFF", " handoff "} {
		if !IsTransferDestination(s) {
			t.Errorf("%q should be recognised as a transfer", s)
		}
	}
	for _, s := range []string{"+4930123456", "", "handoffs", "handoff2"} {
		if IsTransferDestination(s) {
			t.Errorf("%q should NOT be recognised as a transfer", s)
		}
	}
}
