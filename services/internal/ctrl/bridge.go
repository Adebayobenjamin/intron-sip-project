package ctrl

import (
	"context"
	"errors"
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
	HandoffTargets(ctx context.Context, tenantID string) ([]route.HandoffTarget, error)
	AgentConfig(ctx context.Context, planID string) (route.AgentConfig, error)

	// PlanByID is for an OUTBOUND call, which has no dialled DID to resolve —
	// the plan is named by the originate rather than found from a number.
	PlanByID(ctx context.Context, planID string) (route.Plan, error)
}

// connect answers an admitted call and bridges it to where the plan says.
//
// # Order, and why it is this order
//
//  1. Build the dial string. If the plan cannot be routed — no agent config, no
//     handoff target — the call must be refused BEFORE it is answered.
//     Answering first would start carrier billing on a call about to fail, and
//     make the failure look to the caller like a connection that died.
//  2. Answer.
//  3. Start recording. After answer, because there is no media to record
//     before it, and before the bridge so the whole conversation is captured
//     rather than everything after the far end picked up.
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
	watcher *hangupWatcher,
) (platformHungUp bool) {
	params := route.Params{
		CallUUID: ch.UUID,
		TenantID: verdict.TenantID,
		PlanID:   verdict.PlanID,
		CallerID: ch.Caller,
		DID:      rec.DID,
	}

	dial, err := l.dialString(ctx, log, rec, params, verdict)
	if err != nil {
		// Refused pre-answer, deliberately. A plan that cannot be routed is
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

	if verdict.Recording {
		l.startRecording(conn, log, rec, ch.UUID, verdict.TenantID)
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
		cfg, err := l.routes.AgentConfig(ctx, verdict.PlanID)
		if err != nil {
			if errors.Is(err, route.ErrNoAgentConfig) {
				return "", fmt.Errorf("plan %s routes to the AI leg with no agent configured", verdict.PlanID)
			}
			return "", err
		}

		dial, err := route.LiveKit(params, cfg)
		if err != nil {
			return "", err
		}

		rec.LiveKitRoom = route.RoomName(cfg.RoomTemplate, params)
		return dial, nil

	case admission.DestinationHandoff:
		targets, err := l.routes.HandoffTargets(ctx, verdict.TenantID)
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
		return "", fmt.Errorf("plan %s has destination %q, which cannot be routed",
			verdict.PlanID, verdict.Destination)
	}
}

// startRecording writes the call to disk on the FreeSWITCH node.
//
// Failure is NOT fatal to the call. A conversation that happens without being
// recorded is worth a loud log line; refusing to connect a caller because a
// disk is full is not a trade anyone would choose.
//
// RECORD_STEREO puts each leg on its own channel, which is what makes a
// recording useful afterwards — a mixed-down mono file cannot be separated
// back into who said what.
func (l *Listener) startRecording(conn *esl.Conn, log *slog.Logger, rec *calls.Record, uuid, tenantID string) {
	path := route.RecordingPath(l.cfg.RecordingPath, tenantID, uuid)

	if err := conn.SetVar(uuid, "RECORD_STEREO", "true"); err != nil {
		log.Warn("could not set stereo recording", "error", err)
	}

	if _, err := conn.Execute(uuid, "record_session", path); err != nil {
		log.Error("recording failed to start; the call continues unrecorded",
			"path", path, "error", err)
		return
	}

	// Recorded even though nothing ships the file yet: the uploader is not
	// deployed and recording_bucket is empty. The path is the contract between
	// FreeSWITCH, the eventual uploader and this record, and fixing it now is
	// cheaper than reconciling three systems later.
	rec.RecordingURL = path
	log.Info("recording started", "path", path)
}
