package ctrl

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/esl"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/route"
)

// Routes is the routing config the store provides.
type Routes interface {
	// HandoffTargets returns the line's overrides, or the tenant's defaults,
	// with the CALL'S trunk for an E.164 target to leave by.
	HandoffTargets(ctx context.Context, tenantID string, trafficConfigID, trunkID int64) ([]route.HandoffTarget, error)

	// TrafficConfigByID is for an OUTBOUND call, which has no dialled number
	// of ours to resolve — the line is named by the originate rather than
	// found from a number.
	TrafficConfigByID(ctx context.Context, id int64) (route.Line, error)
}

// connect answers an admitted call and bridges it to where the line says.
//
// # Order, and why it is this order
//
//  1. Build the dial string. If the line cannot be routed — no handoff target,
//     no trunk for an E.164 one — the call must be refused BEFORE it is answered.
//     Answering first would start carrier billing on a call about to fail, and
//     make the failure look to the caller like a connection that died.
//  2. Answer.
//  3. Start or stop recording, as recordingActionFor decides. After answer,
//     because there is no media to record before it, and before the bridge so
//     the whole conversation is captured — or, on a handoff, so none of the
//     human half is.
//  4. Bridge.
//
// Returns whether THIS SERVICE hung the call up. That matters for the record:
// a hangup we issued already has a cause and a responsible party, and letting
// FreeSWITCH's NORMAL_CLEARING overwrite it would erase the reason a call was
// refused — the one field anyone asks about afterwards.
func (l *Listener) connect(
	ctx context.Context,
	conn *esl.Conn,
	log *slog.Logger,
	rec *calls.Record,
	ch esl.ChannelData,
	verdict admission.Verdict,
	recording recordingAction,
	watcher *hangupWatcher,
) (platformHungUp bool) {
	params := route.Params{
		CallUUID: ch.UUID,
		TenantID: verdict.TenantID,
		CallerID: ch.Caller,
		DID:      rec.DID,
	}

	dial, err := l.dialString(ctx, log, rec, params, verdict)
	if err != nil {
		// Refused pre-answer, deliberately. A line that cannot be routed is
		// our misconfiguration, so the carrier gets a temporary failure rather
		// than a rejection: the number is valid and will work once someone
		// finishes setting it up.
		log.Error("cannot route this call; refusing before answer", "error", err)
		rec.Outcome = "unroutable"
		l.hangup(conn, log, rec, ch.UUID, causeTemporary)
		return true
	}

	// An OUTBOUND channel is already answered when this socket arrives: the
	// socket application runs on the answered leg, so the far end has picked
	// up by definition. Answering again is at best a no-op and at worst
	// confuses the channel state.
	if ch.IntronDirection != "outbound" {
		if _, err := conn.Execute(ch.UUID, "answer", ""); err != nil {
			log.Error("answer failed", "error", err)
			rec.Outcome = "answer_failed"
			l.hangup(conn, log, rec, ch.UUID, causeTemporary)
			return true
		}
	}

	answeredAt := time.Now()
	rec.AnswerAt = answeredAt

	if err := l.ledger.SetCallState(ctx, ch.UUID, calls.StateAnswered, answeredAt); err != nil {
		// The call is up. A ledger write failing does not justify dropping it —
		// the caller is connected and the record is reconstructed at hangup.
		log.Warn("could not record the answer", "error", err)
	}

	switch recording {
	case recordingStart:
		l.startRecording(conn, log, rec, ch.UUID, verdict.TenantID)
	case recordingStop:
		l.stopRecording(conn, log, ch.UUID, verdict.TenantID)
	}

	log.Info("bridging", "destination", string(verdict.Destination))

	// -----------------------------------------------------------------------
	// BRIDGE, THEN WAIT FOR IT TO ACTUALLY FINISH.
	//
	// The registration comes first, then the command, then the wait — and that
	// order is the whole correctness argument. See expectApp.
	//
	// WHAT THIS REPLACES, AND WHY IT MATTERED. This used to be a bare
	// Execute() whose comment claimed "bridge blocks until the far end hangs
	// up". It does not, and never did: the socket is async, so the reply means
	// FreeSWITCH ACCEPTED the command, not that the application finished. The
	// ESL layer says so directly (see esl.Execute) — the belief was only ever
	// here.
	//
	// So this function returned within milliseconds of the bridge STARTING.
	// The caller then waited its couple of seconds for a hangup event, saw
	// none, and wrote the call off as finished. Observed on a real 30-second
	// call in beta:
	//
	//     08:01:30.445  bridging
	//     08:01:32.451  call ended  outcome="bridged"  total_seconds=2
	//
	// Three consequences, none of which look like a bug in this line:
	//
	//   every CDR recorded ~2 seconds       billing and reporting wrong
	//   the concurrency slot freed at ~2s   a cap of 3 admitting far more
	//   hangup_cause always empty           the hangup arrived long after
	//                                       anyone was still listening
	//
	// It also made transfer detection meaningless. "No hangup seen and the
	// channel still exists" is true of EVERY call two seconds in, so the check
	// in call.go would have called every ordinary call a transfer.
	// -----------------------------------------------------------------------
	bridgeID := l.ids.New()
	bridged := watcher.expectApp(bridgeID)

	if _, err := conn.ExecuteID(ch.UUID, "bridge", dial, bridgeID); err != nil {
		log.Error("bridge failed", "error", err)
		rec.Outcome = "bridge_failed"
		l.hangup(conn, log, rec, ch.UUID, causeTemporary)
		return true
	}

	if err := l.ledger.SetCallState(ctx, ch.UUID, calls.StateBridged, answeredAt); err != nil {
		log.Warn("could not record the bridge", "error", err)
	}

	// Blocks for the length of the call. Three ways out, and the two that are
	// not the bridge finishing both need to be here:
	//
	//   bridged    the normal one — the far end hung up, or the attempt failed
	//   hungUp     the channel died without the bridge reporting completion
	//   ctx        the service is shutting down; the deferred CDR write still
	//              runs, so the call is recorded rather than lost
	select {
	case <-bridged:
	case <-watcher.hungUp():
		log.Info("channel hung up before the bridge reported completion")
	case <-ctx.Done():
		log.Warn("shutting down with the bridge still up")
	}

	// Who ended a bridged call is NOT knowable from here — the bridge
	// returning only says it ended. It is read off CHANNEL_HANGUP_COMPLETE by
	// the caller of this function, which is the only place the answer exists.
	rec.Outcome = "bridged"
	return false
}

// dialString picks the destination and builds it.
func (l *Listener) dialString(
	ctx context.Context,
	log *slog.Logger,
	rec *calls.Record,
	params route.Params,
	verdict admission.Verdict,
) (string, error) {
	switch verdict.Destination {
	case admission.DestinationAI:
		dial, err := route.LiveKit(params)
		if err != nil {
			return "", err
		}

		rec.LiveKitRoom = params.CallUUID
		return dial, nil

	case admission.DestinationHandoff:
		targets, err := l.routes.HandoffTargets(ctx,
			verdict.TenantID, verdict.TrafficConfigID, verdict.TrunkID)
		if err != nil {
			return "", err
		}

		dial, err := route.Handoff(params, targets)
		if err != nil {
			return "", err
		}

		// The first target is what the call is expected to reach. If failover
		// takes it elsewhere the bridge result says so, but recording the
		// intent is better than recording nothing.
		rec.HandoffTargetID = targets[0].ID
		log.Info("handoff", "targets", len(targets), "first", targets[0].Destination)
		return dial, nil

	default:
		return "", fmt.Errorf("traffic config %d has destination %q, which cannot be routed",
			verdict.TrafficConfigID, verdict.Destination)
	}
}
