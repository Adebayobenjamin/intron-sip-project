package ctrl

import (
	"errors"
	"strings"
	"testing"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/route"
)

// handoffRoutes gives the tenant somewhere to send a human handoff. Without
// targets the call is admitted and then refused as unroutable, which passes a
// test that only looks at the record.
func handoffRoutes() *fakeRoutes {
	return &fakeRoutes{targets: []route.HandoffTarget{
		{ID: 7, Kind: route.KindSIPURI, Destination: "sip:queue@sbc.client.example"},
	}}
}

// assertBridged fails unless the call was actually put through to someone.
func assertBridged(t *testing.T, cmds []string) {
	t.Helper()
	if !strings.Contains(strings.Join(cmds, "\n"), "execute-app-name: bridge") {
		t.Fatalf("the call was never bridged; commands:\n%s", strings.Join(cmds, "\n"))
	}
}

// transferredChannel is the leg that comes BACK after a REFER: a new socket,
// the reserved word as its destination, and the SAME uuid as the call it
// continues.
func transferredChannel() channelData {
	ch := defaultChannel()
	ch.destination = TransferDestination
	return ch
}

func liveCall(uuid string) *fakeLive {
	return &fakeLive{call: calls.Live{
		CallUUID: uuid,
		TenantID: "t_test",
		PlanID:   "p_ai",
		DID:      "+13343453663",
		Caller:   "+2349030622835",
		State:    "bridged",
	}}
}

// THE TEST FOR WHAT BETA ACTUALLY DID.
//
// FreeSWITCH does not dial a Refer-To directly — it looks the user part up in
// the dialplan, so the transferred call comes back as a new inbound call
// dialling the literal word `handoff`. Resolved as a number that is nobody's
// DID, which is what beta did on the first real transfer:
//
//	call arrived    did="handoff"
//	refusing call   reason="unknown_did"  cause="UNALLOCATED_NUMBER"
//
// The caller was hung up on at the moment they asked for a human.
func TestHandoff_TransferredLegIsAdmittedNotResolvedAsANumber(t *testing.T) {
	ledger := &fakeLedger{}
	ch := transferredChannel()
	live := liveCall(ch.uuid)

	// The resolver would refuse `handoff`, and must never be asked.
	resolver := &stubResolver{err: errors.New("resolver must not be consulted for a transfer")}

	addr := startListenerLive(t, resolver, ledger, handoffRoutes(), &fakeProbe{}, live)
	fs := dialFakeFS(t, addr, ch)
	cmds := fs.serveCallThenHangup(ch.uuid, "NORMAL_CLEARING", "recv_bye")

	rec := ledger.waitForRecord(t)

	if rec.TenantID != "t_test" {
		t.Errorf("TenantID = %q, want t_test read from the call in progress", rec.TenantID)
	}
	if got := live.lookups(); len(got) != 1 || got[0] != ch.uuid {
		t.Errorf("looked up %v, want exactly [%s] — the uuid is the only handle "+
			"this leg has on the call it continues", got, ch.uuid)
	}

	// It must have been bridged to a human, not refused.
	assertBridged(t, cmds)
	if !strings.Contains(strings.Join(cmds, "\n"), "sbc.client.example") {
		t.Error("bridged somewhere other than the tenant's handoff target")
	}
}

// The record must go on naming the number the caller dialled. Left as
// `handoff`, every transferred call would be filed against a destination no
// tenant owns, and per-number reporting would show calls vanishing from the
// DID they arrived on.
func TestHandoff_RecordKeepsTheOriginalDialledNumber(t *testing.T) {
	ledger := &fakeLedger{}
	ch := transferredChannel()

	addr := startListenerLive(t, &stubResolver{}, ledger, handoffRoutes(), &fakeProbe{}, liveCall(ch.uuid))
	fs := dialFakeFS(t, addr, ch)
	cmds := fs.serveCallThenHangup(ch.uuid, "NORMAL_CLEARING", "recv_bye")
	assertBridged(t, cmds)

	rec := ledger.waitForRecord(t)
	if rec.DID != "+13343453663" {
		t.Errorf("DID = %q, want the number originally dialled", rec.DID)
	}
}

// Both goroutines write this row and either may land last, so they must agree.
// If this leg recorded `bridged`, the label would depend on which finished
// first — see the note in call.go.
func TestHandoff_LegRecordsItselfAsTransferred(t *testing.T) {
	ledger := &fakeLedger{}
	ch := transferredChannel()

	addr := startListenerLive(t, &stubResolver{}, ledger, handoffRoutes(), &fakeProbe{}, liveCall(ch.uuid))
	fs := dialFakeFS(t, addr, ch)
	cmds := fs.serveCallThenHangup(ch.uuid, "NORMAL_CLEARING", "recv_bye")
	assertBridged(t, cmds)

	rec := ledger.waitForRecord(t)
	if rec.Outcome != outcomeTransferred {
		t.Errorf("Outcome = %q, want %q — the other leg writes that value too, "+
			"and disagreeing makes the label depend on who finished last",
			rec.Outcome, outcomeTransferred)
	}
}

// A `handoff` with no call in progress is not a transfer. Something dialled the
// reserved word directly, or the original was finalised early — either way
// there is no call to continue and no tenant to bill.
func TestHandoff_RefusedWhenNoCallIsInProgress(t *testing.T) {
	ledger := &fakeLedger{}
	ch := transferredChannel()
	live := &fakeLive{err: calls.ErrNoLiveCall}

	addr := startListenerLive(t, &stubResolver{}, ledger, &fakeRoutes{}, &fakeProbe{}, live)
	fs := dialFakeFS(t, addr, ch)
	cmds := fs.serveUntilClose()

	if joined := strings.Join(cmds, "\n"); strings.Contains(joined, "execute-app-name: bridge") {
		t.Errorf("bridged a transfer with no call behind it:\n%s", joined)
	}
}

// Without the lookup wired in, a transferred leg cannot find its call. It must
// refuse rather than guess a tenant.
func TestHandoff_RefusedWhenLiveLookupIsUnavailable(t *testing.T) {
	ledger := &fakeLedger{}
	ch := transferredChannel()

	addr := startListenerLive(t, &stubResolver{}, ledger, &fakeRoutes{}, &fakeProbe{}, nil)
	fs := dialFakeFS(t, addr, ch)
	cmds := fs.serveUntilClose()

	if joined := strings.Join(cmds, "\n"); strings.Contains(joined, "execute-app-name: bridge") {
		t.Errorf("bridged a transfer with no way to identify the tenant:\n%s", joined)
	}
}

// An ordinary call must still go through admission. A transfer check that
// caught everything would route every call to a human.
func TestHandoff_OrdinaryCallStillGoesThroughAdmission(t *testing.T) {
	ledger := &fakeLedger{}
	live := &fakeLive{err: errors.New("must not be consulted for an ordinary call")}

	ch := defaultChannel() // a real DID, not the reserved word
	addr := startListenerLive(t, &stubResolver{verdict: aiPlan()}, ledger, &fakeRoutes{}, &fakeProbe{}, live)
	fs := dialFakeFS(t, addr, ch)
	fs.serveCallThenHangup(ch.uuid, "NORMAL_CLEARING", "recv_bye")

	rec := ledger.waitForRecord(t)
	if rec.Outcome == outcomeTransferred {
		t.Error("an ordinary call was treated as a transfer")
	}
	if n := len(live.lookups()); n != 0 {
		t.Errorf("the live-call lookup ran %d time(s) on an ordinary call", n)
	}
}

// A handoff the tenant cannot take must not be recorded as one that happened.
//
// Seen on the first real transfer in beta: `t_test` had no enabled handoff
// target, the leg was refused before answer, and the record still read
// outcome="transferred" with total_seconds=0. The SQL guard then pinned that
// value against the other leg's write, so the wrong label was the one that
// survived — a failed handoff counted as a successful one.
func TestHandoff_FailedHandoffIsNotRecordedAsTransferred(t *testing.T) {
	ledger := &fakeLedger{}
	ch := transferredChannel()

	// The tenant has nowhere to send it — no targets at all.
	addr := startListenerLive(t, &stubResolver{}, ledger, &fakeRoutes{}, &fakeProbe{}, liveCall(ch.uuid))
	fs := dialFakeFS(t, addr, ch)
	cmds := fs.serveUntilClose()

	if strings.Contains(strings.Join(cmds, "\n"), "execute-app-name: bridge") {
		t.Fatal("bridged despite the tenant having no handoff target")
	}

	rec := ledger.waitForRecord(t)
	if rec.Outcome == outcomeTransferred {
		t.Errorf("Outcome = %q; the handoff was refused and never connected",
			rec.Outcome)
	}
	if rec.Outcome != "unroutable" {
		t.Errorf("Outcome = %q, want unroutable — the reason the handoff could "+
			"not happen is the one field anyone would ask about", rec.Outcome)
	}
}
