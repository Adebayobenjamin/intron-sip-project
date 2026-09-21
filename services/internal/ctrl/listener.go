// Package ctrl is the per-call control plane: it accepts the socket
// FreeSWITCH opens for each call, decides what happens, and drives the call
// for its whole life.
package ctrl

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/shared/ids"
)

// Resolver is admission, narrowed to what a call needs. Declared here so this
// package can be tested without a database behind it.
type Resolver interface {
	Resolve(ctx context.Context, did string) (admission.Verdict, error)
}

// Ledger is the call-state half of the store, narrowed the same way.
//
// Claim returns calls.ErrAtCapacity when the tenant is full — a distinct error
// rather than a boolean, because the caller answers it with a different SIP
// cause and a carrier treats the two differently.
type Ledger interface {
	ClaimCall(ctx context.Context, c calls.Claim) error
	Heartbeat(ctx context.Context, uuid string) error
	SetCallState(ctx context.Context, uuid, state string, answeredAt time.Time) error
	FinishCall(ctx context.Context, r calls.Record) error
}

// Config is what the listener needs.
type Config struct {
	Addr string

	// RecordingPath is where FreeSWITCH writes recordings, on its own node.
	RecordingPath string

	// Keepalive is applied to the LISTENER, so every accepted socket inherits
	// it. See the note in Listen — this is the setting calls quietly die
	// without.
	Keepalive time.Duration

	// Node identifies this app instance in logs and on call records.
	Node string

	// HangupWait is how long to wait for CHANNEL_HANGUP_COMPLETE before
	// settling for the earlier, less populated CHANNEL_HANGUP. Zero uses the
	// default; tests set it low so the suite is not mostly sleeping.
	HangupWait time.Duration

	// TransferPoll is how often a transferred call is re-checked. Zero uses
	// the default, and tests set it low for the same reason as above — a suite
	// slow enough to skip stops being a check.
	TransferPoll time.Duration
}

// transferPollInterval resolves the configured poll, falling back to the
// default.
func (c Config) transferPollInterval() time.Duration {
	if c.TransferPoll > 0 {
		return c.TransferPoll
	}
	return transferPoll
}

// hangupWait resolves the configured wait, falling back to the default.
func (c Config) hangupWait() time.Duration {
	if c.HangupWait > 0 {
		return c.HangupWait
	}
	return completeWait
}

// Listener accepts outbound-ESL connections from FreeSWITCH.
type Listener struct {
	cfg      Config
	resolver Resolver
	ledger   Ledger
	routes   Routes
	probe    ChannelProbe
	live     LiveCalls
	log      *slog.Logger

	// ids tags applications so their completion events can be matched back.
	// Not injected: these identifiers never leave the process, so there is
	// nothing for a test to want to control about them.
	ids ids.Generator

	// active counts calls in flight, for the shutdown log. Knowing whether a
	// restart ended three calls or none is the difference between an incident
	// and a non-event.
	active atomic.Int64

	wg sync.WaitGroup
}

// New builds a Listener.
//
// probe and live may both be nil, and they disable the two halves of transfer
// handling independently:
//
//	probe nil   a transferred call cannot be told from one that ended, so the
//	            original leg finalises at the transfer — wrong for transfers,
//	            but not worse than refusing to start.
//	live nil    the leg that comes BACK from a transfer cannot find the call it
//	            continues, and is refused as an unknown number.
func New(
	cfg Config,
	resolver Resolver,
	ledger Ledger,
	routes Routes,
	probe ChannelProbe,
	live LiveCalls,
	log *slog.Logger,
) *Listener {
	return &Listener{
		cfg:      cfg,
		resolver: resolver,
		ledger:   ledger,
		routes:   routes,
		probe:    probe,
		live:     live,
		log:      log,
	}
}

// Run accepts until ctx is cancelled, then stops accepting and waits for calls
// in flight to finish.
//
// # The keepalive
//
// net.ListenConfig sets SO_KEEPALIVE on every accepted connection, and this is
// THE most important line in the service.
//
// FreeSWITCH holds this socket open for the entire call with `async full`. A
// caller on hold with music generates no events this connection subscribed to,
// so the socket goes genuinely silent for minutes. Any middlebox in the path
// reaps an idle flow, and it does so with no FIN — so neither end learns the
// connection is gone. The call drops at a suspiciously round interval, which
// reads as a media problem rather than a transport one.
//
// # The shutdown
//
// Stopping this service ends the calls it is carrying. There is no drain: one
// app instance, no load balancer, and FreeSWITCH's socket application takes a
// single target with no failover. The wait below is not a drain either — it is
// the chance for calls to end cleanly and their state to be written, instead
// of the process vanishing mid-write.
func (l *Listener) Run(ctx context.Context) error {
	lc := net.ListenConfig{KeepAlive: l.cfg.Keepalive}

	ln, err := lc.Listen(ctx, "tcp", l.cfg.Addr)
	if err != nil {
		return err
	}

	l.log.Info("esl listening",
		"addr", ln.Addr().String(),
		"keepalive", l.cfg.Keepalive.String())

	// Close the listener when the context ends so Accept returns rather than
	// blocking forever on a socket nobody will connect to again.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}

			// A transient accept error — a file descriptor limit, most
			// likely — must not kill the listener. Every call that arrives
			// while it is down is lost, and the condition usually clears.
			l.log.Error("accept failed", "error", err)
			continue
		}

		l.wg.Add(1)
		go func() {
			defer l.wg.Done()

			l.active.Add(1)
			defer l.active.Add(-1)

			l.handle(ctx, conn)
		}()
	}

	if n := l.active.Load(); n > 0 {
		l.log.Warn("shutting down with calls in progress; they will end", "calls", n)
	}

	l.wg.Wait()
	l.log.Info("esl listener stopped")
	return nil
}

// Active reports calls in flight, for the health endpoint and the shutdown log.
func (l *Listener) Active() int64 { return l.active.Load() }
