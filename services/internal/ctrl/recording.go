package ctrl

import (
	"log/slog"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/esl"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/route"
)

// recordingAction is what a leg does about recording before it bridges.
type recordingAction int

const (
	// recordingLeave does nothing: either nothing is to be recorded, or a
	// recording already running should simply carry on.
	recordingLeave recordingAction = iota
	recordingStart
	recordingStop
)

func (a recordingAction) String() string {
	switch a {
	case recordingStart:
		return "start"
	case recordingStop:
		return "stop"
	default:
		return "leave"
	}
}

// recordingActionFor decides, from the call's snapshotted mode, what this leg
// does. Pure, so every row of the table below is a test.
//
// # Why a transfer has to STOP the recording, not merely not start one
//
// The recording runs on the A-leg — the caller's channel — as a media bug
// attached by record_session. A REFER transfers that same channel back into
// the dialplan rather than creating a new one, and the bug goes with it. So a
// recording started for the AI conversation keeps running through the human
// handoff unless something stops it.
//
// The previous code believed the opposite: the transferred leg built its
// verdict with `Recording: false` and a comment saying that not carrying the
// setting over kept the human half unrecorded. Not starting a second recording
// was true; the first one was still running. ai_only was never enforced.
//
//	mode      destination  transferred leg?   action
//	none      any          any                leave
//	ai_only   ai           no                 start
//	ai_only   handoff      no                 leave   human from the start: no AI half
//	ai_only   any          yes                stop    the human is taking over
//	full      ai/handoff   no                 start
//	full      any          yes                leave   already running, and must carry on
func recordingActionFor(mode admission.RecordingMode, dest admission.Destination, transferred bool) recordingAction {
	switch mode {
	case admission.RecordingAIOnly:
		if transferred {
			return recordingStop
		}
		if dest == admission.DestinationAI {
			return recordingStart
		}
	case admission.RecordingFull:
		if !transferred {
			return recordingStart
		}
	}
	return recordingLeave
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
	// deployed. The path is the contract between FreeSWITCH, the eventual
	// uploader and this record, and fixing it now is cheaper than reconciling
	// three systems later.
	rec.RecordingURL = path
	log.Info("recording started", "path", path)
}

// stopRecording ends the recording the AI leg started, before the human leg is
// bridged.
//
// The path is derived rather than remembered: it is a pure function of the
// tenant and the channel UUID, and the transferred leg shares both with the
// leg that started it.
//
// A failure here IS worth an error rather than a warning. The call continues —
// dropping a caller mid-handoff is worse — but the consequence is a human
// conversation recorded against the line's policy, which someone will need to
// know about and delete.
func (l *Listener) stopRecording(conn *esl.Conn, log *slog.Logger, uuid, tenantID string) {
	path := route.RecordingPath(l.cfg.RecordingPath, tenantID, uuid)

	if _, err := conn.Execute(uuid, "stop_record_session", path); err != nil {
		log.Error("could not stop the AI-leg recording; the human conversation may be recorded "+
			"against the line's ai_only policy", "path", path, "error", err)
		return
	}
	log.Info("recording stopped for the human handoff", "path", path)
}
