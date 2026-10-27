package ctrl

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/esl"
)

// ChannelProbe asks FreeSWITCH whether a channel is still up.
//
// Answered over the INBOUND ESL connection, not the per-call socket — the
// whole point is to ask about a channel whose socket may already be gone.
type ChannelProbe interface {
	ChannelExists(ctx context.Context, uuid string) (bool, error)
}

// Transfer watch timings.
const (
	// transferPoll is how often a transferred call is checked. Nobody is
	// waiting on the answer: it only decides when the record is completed, so
	// a few seconds of imprecision on end_at costs nothing.
	transferPoll = 5 * time.Second

	// transferMaxWatch bounds the watch. A call that outlives this is either
	// genuinely enormous or a channel FreeSWITCH never cleaned up, and holding
	// a goroutine and a concurrency slot forever is the worse of the two
	// failures. The sweeper reclaims the slot either way.
	transferMaxWatch = 4 * time.Hour
)

// wasTransferred reports whether a call that stopped bridging is still alive.
//
// # How a transfer is told apart from a hangup
//
// Both look identical from inside the controller: `bridge` returns. The
// difference is what happens next — a call that ended produces
// CHANNEL_HANGUP_COMPLETE, and a call that was transferred produces nothing,
// because the channel is still up and talking to someone else.
//
// So "the bridge ended and no hangup event arrived" is the signal, and this
// confirms it by asking FreeSWITCH directly. Asking matters: a hangup event
// can also be missed because the socket died or `linger` was too short, and
// treating that as a transfer would leave the record open on a call that is
// long over.
func (l *Listener) wasTransferred(ctx context.Context, log *slog.Logger, uuid string, sawHangup bool) bool {
	if sawHangup || l.probe == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	alive, err := l.probe.ChannelExists(ctx, uuid)
	if err != nil {
		// Cannot tell. Treat it as ended, which is the conservative answer:
		// the record is written now rather than held open on a call that may
		// have finished, and the slot is released rather than leaked.
		log.Warn("could not check whether the channel is still up; treating the call as ended",
			"error", err)
		return false
	}

	return alive
}

// watchTransferred waits for a transferred call to really end.
//
// # Why this exists
//
// Without it the controller finalises at the moment of transfer: the record
// says the call ended when the AI leg did, and the concurrency slot is
// released while the caller is still talking to a human. A cap that frees up
// early is a cap that permits more calls than it should — wrong in the
// permissive direction, which is the direction that costs money.
//
// The heartbeat keeps running throughout, because the call still holds its
// slot and the sweeper would otherwise reclaim it.
func (l *Listener) watchTransferred(ctx context.Context, log *slog.Logger, rec *calls.Record, uuid string) {
	log.Info("call transferred; the channel is still up, watching for the real ending")

	rec.Outcome = outcomeTransferred

	// The transfer is not an ending, so anything recorded about how the call
	// finished belongs to the leg that just ended rather than to the call.
	// Clearing it lets the final write own those fields.
	rec.HangupCause = ""
	rec.HangupBy = ""
	rec.SIPCode = 0

	deadline := time.Now().Add(transferMaxWatch)
	ticker := time.NewTicker(l.cfg.transferPollInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Shutting down. The call outlives this process; the record is
			// completed with what is known, and the sweeper releases the slot.
			log.Warn("shutting down while a transferred call is still up")
			rec.EndAt = time.Now()
			return

		case <-ticker.C:
			probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			alive, err := l.probe.ChannelExists(probeCtx, uuid)
			cancel()

			if err != nil {
				// Keep watching. A transient failure to reach the switch is
				// not evidence the call ended, and ending the record on it
				// would report a duration that is simply wrong.
				log.Warn("could not check the transferred channel; still watching", "error", err)
				continue
			}

			if !alive {
				rec.EndAt = time.Now()
				log.Info("transferred call ended",
					"total_seconds", int(rec.EndAt.Sub(rec.InviteAt).Seconds()))
				return
			}

			if time.Now().After(deadline) {
				// Give up rather than hold a goroutine and a slot forever.
				log.Warn("transferred call is still up after the watch limit; completing the record",
					"limit", transferMaxWatch.String())
				rec.EndAt = time.Now()
				return
			}
		}
	}
}

// outcomeTransferred marks a call that moved from the AI leg to a human.
const outcomeTransferred = "transferred"

// TransferDestination is the Refer-To user that means "hand this to a human".
//
// Reserved rather than free-form: it says THAT a transfer is wanted, not where
// to. Where comes from the tenant's handoff targets.
const TransferDestination = "handoff"

// IsTransferDestination reports whether a dialled number is the reserved word
// rather than a real DID.
//
// A transferred call re-entering the dialplan arrives looking like a new
// inbound call, and this is what stops the controller resolving `handoff` as a
// phone number — which would correctly find nothing and refuse a call that is
// already connected to a caller.
func IsTransferDestination(did string) bool {
	return strings.EqualFold(strings.TrimSpace(did), TransferDestination)
}

// LiveCalls reads the call a UUID still holds a slot for. Separate from Ledger
// because only the transfer path needs it, and a nil value simply means
// transferred legs are not recognised.
type LiveCalls interface {
	LiveCall(ctx context.Context, uuid string) (calls.Live, error)
}

// transferVerdict admits the leg that comes back after a REFER.
//
// It cannot go through admission at all. The dialled number is the reserved
// word, which belongs to no tenant and matches no DID or range — putting it
// through the resolver produces `unknown_did` and a 404, which is exactly what
// beta did on the first real transfer:
//
//	call arrived    did="handoff"
//	refusing call   reason="unknown_did"  cause="UNALLOCATED_NUMBER"
//
// So the tenant comes from the call already in progress under this UUID. That
// works because FreeSWITCH transfers the EXISTING channel rather than creating
// one, so the UUID is a handle on the call this leg is a continuation of.
//
// It also means a transferred call CANNOT be admitted for a tenant other than
// the one whose call it already is — the identity is read from a row this
// service wrote itself, not from anything the network supplied.
func (l *Listener) transferVerdict(
	ctx context.Context,
	log *slog.Logger,
	ch esl.ChannelData,
) (admission.Verdict, calls.Live) {
	if l.live == nil {
		log.Error("a transfer arrived but this service cannot look up live calls")
		return admission.Verdict{
			Admitted: false,
			Reason:   admission.ReasonLookupFailed,
		}, calls.Live{}
	}

	lookupCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()

	live, err := l.live.LiveCall(lookupCtx, ch.UUID)
	switch {
	case errors.Is(err, calls.ErrNoLiveCall):
		// Nothing is in progress under this UUID, so there is no call to
		// continue. Either the original was finalised early, or something
		// dialled the reserved word without having been transferred. Refused
		// as an unknown number, which is what it is.
		log.Warn("transfer for a call that is not in progress; refusing")
		return admission.Verdict{
			Admitted: false,
			Reason:   admission.ReasonUnknownDID,
		}, calls.Live{}

	case err != nil:
		log.Error("could not read the call being transferred", "error", err)
		return admission.Verdict{
			Admitted: false,
			Reason:   admission.ReasonLookupFailed,
		}, calls.Live{}
	}

	log.Info("transfer accepted",
		"tenant", live.TenantID,
		"original_did", live.DID)

	return admission.Verdict{
		Admitted:        true,
		TenantID:        live.TenantID,
		TrafficConfigID: live.TrafficConfigID,
		Destination:     admission.DestinationHandoff,

		// The call's own trunk, read from the row the original leg claimed. An
		// E.164 handoff is a new call placed through this carrier — the one the
		// line's numbers live on — which is also why it cannot come from the
		// tenant: a tenant on the platform trunk has no trunk of its own.
		TrunkID: live.TrunkID,

		// The mode snapshotted when the call started, not the line's current
		// value. The transferred leg uses it to STOP an ai_only recording —
		// see recordingActionFor for why stopping, not merely not starting, is
		// what keeps the human half unrecorded.
		RecordingMode: admission.RecordingMode(live.RecordingMode),
	}, live
}
