package ctrl

import (
	"context"
	"log/slog"
	"time"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/route"
)

// Originate worker timings.
const (
	// pollInterval is how often the queue is checked.
	//
	// A quarter second of latency before a call starts dialling is invisible
	// against a ring that takes seconds, and the cost when idle is one indexed
	// lookup against a partial index. LISTEN/NOTIFY would remove even that,
	// but a NOTIFY is lost if the listener is disconnected — so it would need
	// this poll as a backstop anyway, and then there are two mechanisms.
	pollInterval = 250 * time.Millisecond

	// originateTimeout bounds the bgapi call, NOT the call itself. bgapi
	// returns as soon as FreeSWITCH accepts the command; the ring happens
	// afterwards and is observed on the socket.
	originateTimeout = 15 * time.Second
)

// OriginateQueue is the store, narrowed.
type OriginateQueue interface {
	ClaimOriginate(ctx context.Context, claimedBy string) (calls.OriginateRequest, bool, error)
	MarkOriginatePlaced(ctx context.Context, callUUID string) error
	MarkOriginateFailed(ctx context.Context, callUUID, reason string) error
}

// Switch places calls on FreeSWITCH over the inbound ESL connection.
type Switch interface {
	BgAPI(cmd string) (string, error)
}

// Originator dials queued outbound calls.
//
// # How it picks work up
//
// It polls. The control plane cannot dial — only this service holds ESL
// connections — so the handoff is a row in originate_requests, claimed with
// FOR UPDATE SKIP LOCKED. That is what makes it safe for two controllers to
// poll at once: each gets a different row, neither waits on the other, and no
// call is placed twice.
type Originator struct {
	queue    OriginateQueue
	switches Switch
	log      *slog.Logger

	// node identifies which controller claimed a row, so an abandoned claim
	// can be traced to the process that died holding it.
	node string

	// callbackTarget is where FreeSWITCH opens the per-call socket once the
	// far end answers — this service's own DNS name.
	callbackTarget string
	edgeSIPRoute   string
}

// NewOriginator builds the worker.
func NewOriginator(q OriginateQueue, s Switch, node, callbackTarget, edgeSIPRoute string, log *slog.Logger) *Originator {
	return &Originator{
		queue:          q,
		switches:       s,
		node:           node,
		callbackTarget: callbackTarget,
		edgeSIPRoute:   edgeSIPRoute,
		log:            log,
	}
}

// Run claims and places calls until the context ends.
//
// A missing callback target stops the WORKER, not the service. Inbound calls
// do not need it, and taking the whole controller down over a feature nobody
// has configured would turn an unused capability into an outage.
func (o *Originator) Run(ctx context.Context) error {
	if o.callbackTarget == "" {
		o.log.Warn("ESL_CALLBACK_TARGET is not set; outbound originate is disabled " +
			"(inbound calls are unaffected)")
		<-ctx.Done()
		return nil
	}

	// Checked at startup as well as per call. Without it every originate is
	// accepted, marked `placed`, and never reaches the edge — so the failure
	// belongs in the log at boot, not once per silently-lost call.
	if o.edgeSIPRoute == "" {
		o.log.Warn("EDGE_SIP_ROUTE is not set; outbound originate is disabled " +
			"(without it FreeSWITCH would send the INVITE straight at the carrier, " +
			"bypassing the edge, and the call would be dropped at the security group)")
		<-ctx.Done()
		return nil
	}

	o.log.Info("originate worker started",
		"poll", pollInterval.String(),
		"callback", o.callbackTarget,
		"edge_route", o.edgeSIPRoute)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			o.log.Info("originate worker stopped")
			return nil
		case <-ticker.C:
			// Drain rather than taking one per tick, so a burst of queued
			// calls does not dial at four per second.
			for o.once(ctx) {
				if ctx.Err() != nil {
					return nil
				}
			}
		}
	}
}

// once claims and places at most one request. It reports whether there may be
// more.
func (o *Originator) once(ctx context.Context) bool {
	req, ok, err := o.queue.ClaimOriginate(ctx, o.node)
	if err != nil {
		if ctx.Err() == nil {
			o.log.Error("could not claim an originate request", "error", err)
		}
		return false
	}
	if !ok {
		return false
	}

	log := o.log.With(
		"call_uuid", req.CallUUID,
		"tenant", req.TenantID,
		"to", req.ToNumber,
		"from", req.FromDID,
	)

	dial, err := route.Originate(route.OriginateParams{
		CallUUID:        req.CallUUID,
		TenantID:        req.TenantID,
		TrafficConfigID: req.TrafficConfigID,
		FromDID:         req.FromDID,
		ToNumber:        req.ToNumber,
		TrunkHost:       req.TrunkHost,
		TrunkPort:       req.TrunkPort,
		CallbackTarget:  o.callbackTarget,
		EdgeSIPRoute:    o.edgeSIPRoute,
	})
	if err != nil {
		o.fail(ctx, log, req.CallUUID, err.Error())
		return true
	}

	// bgapi, not api. A synchronous originate holds the connection for the
	// whole ring — seconds to tens of seconds — and blocks every command
	// queued behind it on the one connection this service has to the switch.
	job, err := o.switches.BgAPI("originate " + dial)
	if err != nil {
		o.fail(ctx, log, req.CallUUID, err.Error())
		return true
	}

	if err := o.queue.MarkOriginatePlaced(ctx, req.CallUUID); err != nil {
		// The call IS being placed; only the bookkeeping failed. Saying so
		// matters because the row will look stuck in `claimed` forever, and
		// the reason is here rather than in anything the row can show.
		log.Error("call placed but the request could not be marked; the row will look stuck",
			"job", job, "error", err)
		return true
	}

	// Placed means dialled, not answered. What happens next arrives on the
	// socket FreeSWITCH opens when the far end picks up.
	log.Info("outbound call placed", "job", job)
	return true
}

func (o *Originator) fail(ctx context.Context, log *slog.Logger, callUUID, reason string) {
	log.Error("could not place the outbound call", "reason", reason)

	// A detached context: the reason is the whole point of the row, and losing
	// it because a shutdown cancelled the call's context would leave a request
	// that failed for no recorded reason.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if err := o.queue.MarkOriginateFailed(writeCtx, callUUID, reason); err != nil {
		log.Error("could not record the failure", "error", err)
	}
}
