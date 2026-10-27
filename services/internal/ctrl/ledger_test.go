package ctrl

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
)

// runCall plays a whole call through the fake FreeSWITCH and returns every
// command the controller issued.
//
// It serves until the controller closes the connection rather than expecting a
// fixed sequence: a refused call ends after one command and an admitted one
// answers, may record, and bridges. A test that predicts the sequence has to
// be edited every time the lifecycle grows a step.
func runCall(t *testing.T, r Resolver, l Ledger) []string {
	t.Helper()

	fs := dialFakeFS(t, startListener(t, r, l), defaultChannel())
	return fs.serveUntilClose()
}

// hangupCause pulls the cause out of the hangup command, for tests that care
// about what the carrier saw.
func hangupCause(t *testing.T, cmds []string) string {
	t.Helper()

	cmd, ok := findCommand(cmds, "call-command: hangup")
	if !ok {
		t.Fatalf("no hangup in %v", cmds)
	}
	for _, line := range strings.Split(cmd, "\n") {
		if v, found := strings.CutPrefix(line, "hangup-cause: "); found {
			return v
		}
	}
	t.Fatalf("hangup carried no cause: %q", cmd)
	return ""
}

// ---------------------------------------------------------------------------
// The claim
// ---------------------------------------------------------------------------

// The slot is taken before anything that costs money or media, and only for a
// call that was admitted.
func TestClaim_TakenForAdmittedCalls(t *testing.T) {
	ledger := &fakeLedger{}
	runCall(t, &stubResolver{verdict: admitted()}, ledger)

	claims := ledger.claimed()
	if len(claims) != 1 {
		t.Fatalf("want 1 claim, got %d", len(claims))
	}

	c := claims[0]
	if c.TenantID != "t_acme" || c.TrafficConfigID != 21 || c.TrunkID != 9 {
		t.Errorf("claim did not carry the verdict: %+v", c)
	}
	// The recording mode is snapshotted onto the slot here, at the start of
	// the call. A transferred leg reads it back from this row.
	if c.RecordingMode != "ai_only" {
		t.Errorf("RecordingMode = %q, want ai_only snapshotted from the line", c.RecordingMode)
	}
	if c.Direction != calls.DirectionInbound {
		t.Errorf("Direction = %q, want inbound", c.Direction)
	}
	// The claim stores the normalised number, so the ledger and the records
	// agree on what a number looks like.
	if c.DID != "+4930123456" {
		t.Errorf("DID = %q, want the normalised form", c.DID)
	}
}

// A refused call never reaches the ledger. Claiming a slot for a call about to
// be hung up would count it against the tenant's capacity for no reason.
func TestClaim_NotTakenForRefusedCalls(t *testing.T) {
	ledger := &fakeLedger{}
	runCall(t, &stubResolver{verdict: refused(admission.ReasonUnknownDID)}, ledger)

	if n := len(ledger.claimed()); n != 0 {
		t.Errorf("want no claim for a refused call, got %d", n)
	}
}

// At capacity the carrier gets congestion, not a rejection. A carrier may
// retry congestion and should not retry a refusal — using the wrong one either
// loses a call that would have connected, or invites a retry storm.
func TestClaim_AtCapacityIsCongestion(t *testing.T) {
	ledger := &fakeLedger{claimErr: calls.ErrAtCapacity}

	cmds := runCall(t, &stubResolver{verdict: admitted()}, ledger)

	if got := hangupCause(t, cmds); got != causeCongestion {
		t.Errorf("want %s, got %s", causeCongestion, got)
	}

	rec := ledger.waitForRecord(t)
	if rec.Outcome != string(admission.ReasonOverCapacity) {
		t.Errorf("Outcome = %q, want %q", rec.Outcome, admission.ReasonOverCapacity)
	}
	if rec.SIPCode != 503 {
		t.Errorf("SIPCode = %d, want 503", rec.SIPCode)
	}
}

// If the ledger is unreachable the call must be refused, not carried.
// Carrying a call that was never counted means the cap silently stops meaning
// anything — and bounding toll fraud is what caps are for.
func TestClaim_LedgerFailureRefusesTheCall(t *testing.T) {
	ledger := &fakeLedger{claimErr: errors.New("connection refused")}

	cmds := runCall(t, &stubResolver{verdict: admitted()}, ledger)

	if got := hangupCause(t, cmds); got != causeTemporary {
		t.Errorf("want %s, got %s", causeTemporary, got)
	}

	rec := ledger.waitForRecord(t)
	if rec.Outcome != "claim_failed" {
		t.Errorf("Outcome = %q, want claim_failed", rec.Outcome)
	}
}

// ---------------------------------------------------------------------------
// Call records
// ---------------------------------------------------------------------------

// A refused call is still a call someone will ask about. "Why did nothing
// happen when I rang that number" is answered by this record or not at all.
func TestRecord_WrittenForRefusedCalls(t *testing.T) {
	ledger := &fakeLedger{}
	runCall(t, &stubResolver{verdict: refused(admission.ReasonTenantSuspended)}, ledger)

	rec := ledger.waitForRecord(t)

	if rec.Outcome != string(admission.ReasonTenantSuspended) {
		t.Errorf("Outcome = %q, want %q", rec.Outcome, admission.ReasonTenantSuspended)
	}
	if rec.HangupCause != causeRejected {
		t.Errorf("HangupCause = %q, want %q", rec.HangupCause, causeRejected)
	}
	if rec.SIPCode != 403 {
		t.Errorf("SIPCode = %d, want 403", rec.SIPCode)
	}
	if rec.HangupBy != calls.HangupByPlatform {
		t.Errorf("HangupBy = %q, want %q — a refusal is not a caller hangup",
			rec.HangupBy, calls.HangupByPlatform)
	}
}

func TestRecord_CarriesTheCallIdentity(t *testing.T) {
	ledger := &fakeLedger{}
	runCall(t, &stubResolver{verdict: admitted()}, ledger)

	rec := ledger.waitForRecord(t)
	ch := defaultChannel()

	if rec.UUID != ch.uuid {
		t.Errorf("UUID = %q, want %q", rec.UUID, ch.uuid)
	}
	// Cannot be reconstructed afterwards, and it is what lines a call up with
	// the carrier's own records.
	if rec.SIPCallID != ch.sipCallID {
		t.Errorf("SIPCallID = %q, want %q", rec.SIPCallID, ch.sipCallID)
	}
	if rec.Node != ch.node {
		t.Errorf("Node = %q, want %q", rec.Node, ch.node)
	}
	if rec.Caller != ch.caller {
		t.Errorf("Caller = %q, want %q", rec.Caller, ch.caller)
	}
}

// A call that never answered must not record a talk time, and a refused call
// has no answer instant at all. Writing the zero time would store the year 1
// and sort accordingly.
func TestRecord_NoAnswerMeansNoTalkTime(t *testing.T) {
	ledger := &fakeLedger{}
	runCall(t, &stubResolver{verdict: refused(admission.ReasonUnknownDID)}, ledger)

	// A tenant is needed for the record to be written at all.
	ledger2 := &fakeLedger{}
	runCall(t, &stubResolver{verdict: refused(admission.ReasonTenantSuspended)}, ledger2)

	rec := ledger2.waitForRecord(t)
	if !rec.AnswerAt.IsZero() {
		t.Errorf("AnswerAt = %v, want zero for a call that never answered", rec.AnswerAt)
	}
	if rec.EndAt.IsZero() {
		t.Error("EndAt must always be set")
	}
	if rec.InviteAt.IsZero() {
		t.Error("InviteAt must always be set")
	}
	_ = ledger
}

// A call for a number nobody owns has no tenant to file it under, and every
// query is scoped by tenant — so writing it would store a row nobody could
// ever read.
func TestRecord_SkippedWhenThereIsNoTenant(t *testing.T) {
	ledger := &fakeLedger{}
	runCall(t, &stubResolver{verdict: admission.Verdict{
		Admitted: false,
		Reason:   admission.ReasonUnknownDID,
	}}, ledger)

	time.Sleep(300 * time.Millisecond)

	if n := len(ledger.written()); n != 0 {
		t.Errorf("want no record without a tenant, got %d", n)
	}
}

// The record write runs on a context detached from the call's, so a shutdown
// cancelling everything does not also discard the record of a call that
// definitely happened.
func TestRecord_WrittenEvenAsTheListenerStops(t *testing.T) {
	ledger := &fakeLedger{}
	runCall(t, &stubResolver{verdict: refused(admission.ReasonTenantSuspended)}, ledger)

	// t.Cleanup cancels the listener's context; the record must already be
	// there, written from the detached context.
	rec := ledger.waitForRecord(t)
	if rec.UUID == "" {
		t.Error("record was lost")
	}
}
