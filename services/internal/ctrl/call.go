package ctrl

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/esl"
)

// Hangup causes, and the SIP response each produces.
//
// THE CAUSE IS THE ONLY THING THE CARRIER AND THE CDR EVER SEE. A bare hangup
// reads as normal clearing, which makes a call this platform refused
// indistinguishable from one that simply ended — in the carrier's records and
// in ours, weeks later, when someone asks why a number was not working.
const (
	// causeUnallocated → SIP 404. The number is not ours.
	causeUnallocated = "UNALLOCATED_NUMBER"

	// causeRejected → SIP 403. The number IS ours and we are refusing:
	// suspended tenant, inactive plan, a plan configured to refuse.
	causeRejected = "CALL_REJECTED"

	// causeCongestion → SIP 503. At capacity. Distinct from a fault because a
	// carrier may retry congestion and should not retry a rejection.
	causeCongestion = "NORMAL_CIRCUIT_CONGESTION"

	// causeTemporary → SIP 503. Our fault: the database was unreachable, or
	// the number is provisioned with nothing configured behind it.
	causeTemporary = "NORMAL_TEMPORARY_FAILURE"
)

// resolveTimeout bounds admission. On a cache hit this is microseconds; the
// budget exists for the miss that has to reach Postgres while the caller is
// listening to silence.
const resolveTimeout = 2 * time.Second

// lingerSeconds keeps the socket open briefly after the channel ends so the
// final events arrive before it closes. Without it the hangup cause can be
// lost, and it is the field the CDR most needs.
const lingerSeconds = 10

// heartbeatInterval must be comfortably shorter than the sweeper's threshold,
// or a perfectly healthy call has its slot reclaimed between beats.
const heartbeatInterval = 30 * time.Second

// finishTimeout bounds the record write. It runs on a context detached from
// the call's, so a shutdown cancelling everything does not also discard the
// record of a call that definitely happened.
const finishTimeout = 10 * time.Second

// handle drives one call, start to finish.
//
// The call arrives PRE-ANSWER. The dialplan deliberately does not answer it:
// answering starts carrier billing on a call that may turn out to be
// unroutable, and makes a rejection look to the caller like a call that
// connected and then died. So every refusal below happens before answer, and
// the ordering is the whole reason it works.
func (l *Listener) handle(ctx context.Context, rawConn net.Conn) {
	arrived := time.Now()
	remote := rawConn.RemoteAddr().String()

	conn := esl.NewConn(rawConn)
	defer conn.Close()

	// The read loop starts BEFORE the first command, and the ordering is not
	// cosmetic: command replies arrive on the same socket as events, so with
	// nothing reading, `connect` writes its command and then waits for a reply
	// that nobody will ever take off the wire. Every call would time out
	// identically, which reads as FreeSWITCH not answering rather than as this
	// service not listening.
	watcher := newHangupWatcher()
	go func() {
		_ = conn.Serve(func(m *esl.Message) {
			body, err := esl.ParseEventBody(m.Body)
			if err != nil {
				return
			}
			watcher.observe(body)
		})
	}()

	// The channel data arrives as the reply to `connect` and nowhere else.
	reply, err := conn.Connect()
	if err != nil {
		l.log.Error("esl handshake failed", "remote", remote, "error", err)
		return
	}

	ch := esl.ParseChannelData(reply)

	log := l.log.With(
		"call_uuid", ch.UUID,
		"did", ch.DestinationNumber,
		"caller", ch.Caller,
		"fs_node", nodeOrRemote(ch.Node, remote),
	)

	if ch.UUID == "" {
		// Without a UUID no command can be addressed to the channel, so there
		// is nothing this service can do with the call — including hang it up
		// politely. Drop the socket and let the dialplan's fallback take over.
		log.Error("connect reply carried no channel UUID; dropping")
		return
	}

	// Subscribe BEFORE anything else. Miss this and the connection receives
	// the channel data and then nothing — no answer, no hangup, ever. The call
	// would connect, this service would never learn it ended, and the socket
	// would sit open holding a call nobody is on.
	if err := conn.MyEvents(ch.UUID); err != nil {
		log.Error("could not subscribe to channel events", "error", err)
		return
	}

	if err := conn.Linger(lingerSeconds); err != nil {
		// Not fatal. It costs the final events on a call that is otherwise
		// handled correctly, which is worth a line but not a dropped call.
		log.Warn("linger refused; final events may be lost", "error", err)
	}

	log.Info("call arrived")

	// The record is built as the call goes and written exactly once, on every
	// path out of this function. A refused call is still a call someone will
	// ask about.
	rec := calls.Record{
		UUID:      ch.UUID,
		SIPCallID: ch.SIPCallID,
		Direction: directionOf(ch),
		DID:       admission.Normalise(ch.DestinationNumber),
		Caller:    ch.Caller,
		Node:      nodeOrRemote(ch.Node, remote),
		InviteAt:  arrived,
		RingAt:    arrived,

		// HangupBy is deliberately unset. It is filled in by whichever of two
		// things happens: this service hanging the call up, or
		// CHANNEL_HANGUP_COMPLETE saying who did. Defaulting it to "platform"
		// would be a guess, and it would be wrong for exactly the calls that
		// went well.
	}
	defer func() { l.finish(ctx, log, &rec) }()

	// OUTBOUND calls take a different route to a verdict.
	//
	// There is no dialled DID to resolve: the number in the Request-URI is the
	// customer's, not one we own. The tenant and plan come from the channel
	// variables the originate set — which this service wrote itself, so they
	// are trustworthy in a way a header from the network never is.
	outbound := ch.Direction == "outbound" || ch.IntronDirection == "outbound"

	// A TRANSFERRED leg dials the reserved word rather than a number, and is
	// neither an ordinary inbound call nor an outbound one. See
	// transferVerdict for why it cannot go through admission.
	transferred := !outbound && IsTransferDestination(ch.DestinationNumber)

	var verdict admission.Verdict
	switch {
	case transferred:
		var live calls.Live
		verdict, live = l.transferVerdict(ctx, log, ch)

		if verdict.Admitted {
			// Keep naming the number the caller actually dialled. Left as
			// `handoff`, every transferred call in the history would have a
			// destination no tenant owns, and per-number reporting would show
			// calls vanishing from the DID they arrived on.
			rec.DID = live.DID
			if rec.Caller == "" {
				rec.Caller = live.Caller
			}

			// Agreed with the other goroutine rather than coordinated with it.
			//
			// Both legs write this row: the original one is still waiting for
			// the channel to die so it can record the ending, and it writes
			// `transferred` too. The history upsert merges on call_uuid and
			// takes the LAST non-null outcome, so if this leg wrote `bridged`
			// the label would depend on which of the two finished last — the
			// original polls every few seconds, this one finishes the instant
			// the call ends, so it would usually but not always be right.
			//
			// Writing the same value from both ends removes the ordering
			// question instead of answering it.
			log = log.With("transferred", true, "original_did", live.DID)
		}

	case outbound:
		verdict = l.outboundVerdict(ctx, log, ch)

	default:
		verdict = l.decide(ctx, log, ch)
	}

	rec.TenantID = verdict.TenantID
	rec.PlanID = verdict.PlanID
	rec.TrunkID = verdict.TrunkID
	rec.Outcome = string(verdict.Reason)

	if !verdict.Admitted {
		l.refuse(conn, log, &rec, ch.UUID, verdict.Reason)
		watcher.wait(l.cfg.hangupWait())
		return
	}

	log.Info("call admitted",
		"tenant", verdict.TenantID,
		"plan", verdict.PlanID,
		"destination", string(verdict.Destination),
		"recording", verdict.Recording,
		"stale_config", verdict.Stale)

	// An outbound call already holds its slot: the control plane claimed one
	// when the request was queued, so a tenant at its cap cannot fill a queue
	// with calls it may not place. Claiming again here would count it twice.
	if outbound {
		stopBeat := l.heartbeat(ctx, ch.UUID, log)
		defer stopBeat()

		platformHungUp := l.connect(ctx, conn, log, &rec, ch, verdict, watcher)
		info := watcher.wait(l.cfg.hangupWait())
		info.apply(&rec, platformHungUp)
		return
	}

	// Take the slot BEFORE anything that costs money or media. The cap is
	// enforced by the database, not by a count read here — see
	// store.ClaimCall for why the obvious version of that is a race.
	claim := calls.Claim{
		UUID:      ch.UUID,
		TenantID:  verdict.TenantID,
		PlanID:    verdict.PlanID,
		TrunkID:   verdict.TrunkID,
		Direction: rec.Direction,
		DID:       rec.DID,
		Caller:    ch.Caller,
		Node:      rec.Node,
	}

	if err := l.ledger.ClaimCall(ctx, claim); err != nil {
		if errors.Is(err, calls.ErrAtCapacity) {
			rec.Outcome = string(admission.ReasonOverCapacity)
			l.refuse(conn, log, &rec, ch.UUID, admission.ReasonOverCapacity)
			watcher.wait(l.cfg.hangupWait())
			return
		}

		// The ledger is unreachable. Refusing is the only safe answer:
		// carrying a call that was never counted means the cap silently stops
		// meaning anything, and toll fraud is what caps exist to bound.
		log.Error("could not claim a concurrency slot; refusing", "error", err)
		rec.Outcome = "claim_failed"
		l.refuse(conn, log, &rec, ch.UUID, admission.ReasonLookupFailed)
		watcher.wait(l.cfg.hangupWait())
		return
	}

	// Hold the slot for as long as the call lives. A controller that dies
	// leaves its rows claimed with nobody on the calls, and the sweeper
	// reclaims anything that stops beating.
	stopBeat := l.heartbeat(ctx, ch.UUID, log)
	defer stopBeat()

	// Answer and bridge. Everything that can refuse the call happens inside,
	// before the answer, so a call that cannot be routed is never billed.
	// Returns when the bridge has actually ended, not when it started — so
	// everything below runs at the end of the call rather than two seconds
	// into it. See the note on the select in connect().
	platformHungUp := l.connect(ctx, conn, log, &rec, ch, verdict, watcher)

	// Wait for CHANNEL_HANGUP_COMPLETE, which is the event carrying the
	// populated CDR variables. Whatever it reports beats what this service
	// inferred — except when this service was the one that hung up.
	info := watcher.wait(l.cfg.hangupWait())
	info.apply(&rec, platformHungUp)

	// ONLY WHEN THE CALL ACTUALLY REACHED SOMEONE. connect() reports
	// platformHungUp when it refused before answer, and it has already written
	// the reason — `unroutable` for a tenant with no handoff target.
	// Overwriting that with `transferred` would record a handoff that never
	// happened as one that did.
	//
	// Seen on the first real transfer in beta: the tenant had no enabled
	// handoff target, the leg was refused, and the record still said
	// `transferred` with a duration of zero. A failed handoff that reports
	// success is worse than one that reports nothing, because the number it
	// inflates is the one anyone would check.
	if transferred && !platformHungUp {
		// This leg IS the transfer, so it ends as one. connect() sets
		// `bridged` for every call it puts through; overriding here rather
		// than teaching connect() about transfers keeps the bridge path
		// unaware of why it was asked to dial.
		//
		// The same value the original leg writes — see the note above where
		// this leg is admitted.
		rec.Outcome = outcomeTransferred
	}

	// A bridge that ended with no hangup event may mean the call was
	// TRANSFERRED rather than finished — the agent asked for a human, and the
	// channel is still up talking to one. Finalising here would end the record
	// at the transfer and release the concurrency slot on a live call.
	//
	// Not checked on the transferred leg itself: it is the continuation, and a
	// second transfer would arrive as its own leg the same way this one did.
	if !transferred && l.wasTransferred(ctx, log, ch.UUID, info.Seen) {
		l.watchTransferred(ctx, log, &rec, ch.UUID)
	}

	log.Info("hangup observed",
		"cause", info.Cause,
		"disposition", info.Disposition,
		"from_complete_event", info.Complete,
		"seen", info.Seen)
}

// refuse hangs up with the cause the reason maps to, and records it.
func (l *Listener) refuse(conn *esl.Conn, log *slog.Logger, rec *calls.Record, uuid string, reason admission.Reason) {
	cause := causeFor(reason)

	log.Info("refusing call",
		"reason", string(reason),
		"cause", cause,
		"tenant", rec.TenantID)

	l.hangup(conn, log, rec, uuid, cause)
}

func (l *Listener) hangup(conn *esl.Conn, log *slog.Logger, rec *calls.Record, uuid, cause string) {
	rec.HangupCause = cause
	rec.SIPCode = sipCodeFor(cause)
	rec.HangupBy = calls.HangupByPlatform

	if err := conn.Hangup(uuid, cause); err != nil {
		log.Error("hangup failed", "cause", cause, "error", err)
	}
}

// finish writes the record and releases the slot, in one transaction.
//
// It runs on every path out of handle, including the ones that failed early,
// because a call nobody has a record of is the one someone asks about. A
// record with no tenant is dropped rather than written: every query is scoped
// by tenant, so it would be stored and then invisible.
func (l *Listener) finish(ctx context.Context, log *slog.Logger, rec *calls.Record) {
	if rec.EndAt.IsZero() {
		rec.EndAt = time.Now()
	}

	if rec.TenantID == "" {
		// A call for a number nobody owns. There is no tenant to file it
		// under and no tenant who could ever read it.
		log.Info("call ended with no tenant; no record written",
			"outcome", rec.Outcome, "cause", rec.HangupCause)
		return
	}

	// A fresh context: the call's may already be cancelled by a shutdown, and
	// losing the record of a call that definitely happened is the one outcome
	// worth spending part of the grace period on.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()

	if err := l.ledger.FinishCall(writeCtx, *rec); err != nil {
		// Loud, because it is invisible otherwise: the call worked, the caller
		// hung up happy, and the only sign anything went wrong is a CDR that
		// is not there when someone goes looking weeks later.
		log.Error("could not write the call record; it is lost",
			"outcome", rec.Outcome, "error", err)
		return
	}

	log.Info("call ended",
		"outcome", rec.Outcome,
		"cause", rec.HangupCause,
		"total_seconds", int(rec.EndAt.Sub(rec.InviteAt).Seconds()))
}

// heartbeat keeps the claim alive and returns a function that stops it.
func (l *Listener) heartbeat(ctx context.Context, uuid string, log *slog.Logger) func() {
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := l.ledger.Heartbeat(ctx, uuid); err != nil && ctx.Err() == nil {
					// Not fatal to the call — the audio is not going through
					// this. It does mean the sweeper may reclaim the slot
					// while the call is still up, so it is worth a line.
					log.Warn("heartbeat failed; the slot may be swept", "error", err)
				}
			}
		}
	}()

	return cancel
}

func directionOf(ch esl.ChannelData) string {
	if ch.Direction == "outbound" || ch.IntronDirection == "outbound" {
		return calls.DirectionOutbound
	}
	return calls.DirectionInbound
}

// outboundVerdict builds a verdict for a call this service placed.
//
// No admission lookup: the number dialled belongs to the customer, not to us,
// so resolving it as a DID would correctly find nothing and refuse a call we
// asked for. The tenant and plan are read off the channel variables the
// originate set — written by this service, not supplied by the network.
func (l *Listener) outboundVerdict(ctx context.Context, log *slog.Logger, ch esl.ChannelData) admission.Verdict {
	v := admission.Verdict{
		Admitted: true,
		Reason:   admission.ReasonOK,
		TenantID: ch.Tenant,
		PlanID:   ch.Plan,

		// Default for an outbound call with no plan: put the customer with the
		// AI agent, which is what an outbound campaign is for. A plan that
		// says otherwise overrides it below.
		Destination: admission.DestinationAI,
	}

	if ch.Plan == "" {
		log.Info("outbound call with no plan; defaulting to the AI leg")
		return v
	}

	// A plan was named, so its configuration decides what happens — including
	// whether to record, which is not something to guess at on a call that is
	// already connected.
	plan, err := l.routes.PlanByID(ctx, ch.Plan)
	if err != nil {
		log.Warn("could not load the plan for an outbound call; using defaults",
			"plan", ch.Plan, "error", err)
		return v
	}

	v.Destination = admission.Destination(plan.Destination)
	v.Recording = plan.Recording
	return v
}

// sipCodeFor records what the carrier saw, so the record and the carrier's own
// logs can be lined up without anyone re-deriving the mapping.
func sipCodeFor(cause string) int16 {
	switch cause {
	case causeUnallocated:
		return 404
	case causeRejected:
		return 403
	case causeCongestion, causeTemporary:
		return 503
	default:
		return 0
	}
}

// decide resolves admission for a call.
//
// A lookup failure is NOT the same as an unowned number, and conflating them
// is how an outage gets reported to a carrier as "that number does not exist".
// The carrier then stops retrying, and the CDR records a permanent condition
// for what was a transient one.
func (l *Listener) decide(ctx context.Context, log *slog.Logger, ch esl.ChannelData) admission.Verdict {
	resolveCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()

	verdict, err := l.resolver.Resolve(resolveCtx, ch.DestinationNumber)
	if err != nil {
		log.Error("admission lookup failed; refusing as a temporary failure", "error", err)
		return admission.Verdict{Admitted: false, Reason: admission.ReasonLookupFailed}
	}

	if verdict.Stale {
		log.Warn("call routed on stale configuration; the database was unreachable",
			"tenant", verdict.TenantID)
	}

	return verdict
}

// causeFor maps a refusal to the cause the carrier sees.
func causeFor(reason admission.Reason) string {
	switch reason {
	case admission.ReasonUnknownDID, admission.ReasonDIDInactive:
		// Not ours, or withdrawn. 404 is the honest answer and it is what
		// stops a carrier retrying.
		return causeUnallocated

	case admission.ReasonTenantSuspended,
		admission.ReasonPlanInactive,
		admission.ReasonPlanRejects:
		// Ours, and we are refusing on purpose. 403, not 404 — the number
		// exists, and telling a carrier otherwise invites it to be
		// reallocated.
		return causeRejected

	case admission.ReasonOverCapacity:
		return causeCongestion

	case admission.ReasonNoPlan:
		// Provisioned with nothing behind it. OUR misconfiguration, not the
		// caller's problem, so a temporary failure rather than 404 — the
		// number does exist and will work once someone finishes setting it up.
		return causeTemporary

	default:
		return causeTemporary
	}
}

// nodeOrRemote prefers the node name the dialplan set, falling back to the
// source address.
//
// The address only identifies the node because the container runs with
// --network host. Behind Docker's bridge every connection appears to come from
// the gateway, and this would silently start reporting one address for every
// FreeSWITCH in the farm.
func nodeOrRemote(node, remote string) string {
	if node != "" {
		return node
	}
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	return remote
}
