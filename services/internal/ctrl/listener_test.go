package ctrl

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/route"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// stubResolver returns a fixed verdict, so these tests are about the protocol
// and the call lifecycle rather than about admission (which has its own).
type stubResolver struct {
	verdict admission.Verdict
	err     error
	calls   int
	lastDID string
}

func (s *stubResolver) Resolve(_ context.Context, did string) (admission.Verdict, error) {
	s.calls++
	s.lastDID = did
	return s.verdict, s.err
}

// fakeLedger records what the call lifecycle asked of the database.
type fakeLedger struct {
	mu sync.Mutex

	claimErr  error
	finishErr error
	stateErr  error

	claims   []calls.Claim
	records  []calls.Record
	beats    int
	stateSet []string
}

func (f *fakeLedger) ClaimCall(_ context.Context, c calls.Claim) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return f.claimErr
	}
	f.claims = append(f.claims, c)
	return nil
}

func (f *fakeLedger) Heartbeat(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beats++
	return nil
}

func (f *fakeLedger) SetCallState(_ context.Context, _, state string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stateErr != nil {
		return f.stateErr
	}
	f.stateSet = append(f.stateSet, state)
	return nil
}

func (f *fakeLedger) FinishCall(_ context.Context, r calls.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.finishErr != nil {
		return f.finishErr
	}
	f.records = append(f.records, r)
	return nil
}

func (f *fakeLedger) written() []calls.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]calls.Record(nil), f.records...)
}

func (f *fakeLedger) claimed() []calls.Claim {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]calls.Claim(nil), f.claims...)
}

// waitForRecord gives the deferred write a moment to land. The record is
// written after the socket closes, so a test that checks immediately races it.
func (f *fakeLedger) waitForRecord(t *testing.T) calls.Record {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if recs := f.written(); len(recs) > 0 {
			return recs[0]
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("no call record was written")
	return calls.Record{}
}

func admitted() admission.Verdict {
	return admission.Verdict{
		Admitted:    true,
		Reason:      admission.ReasonOK,
		TenantID:    "t_acme",
		PlanID:      "p_main",
		Destination: admission.DestinationAI,
		Recording:   true,
	}
}

func refused(r admission.Reason) admission.Verdict {
	return admission.Verdict{Admitted: false, Reason: r, TenantID: "t_acme"}
}

// startListener runs a Listener on a free port and returns its address.
func startListener(t *testing.T, r Resolver, ledger Ledger) string {
	t.Helper()
	return startListenerWithRoutes(t, r, ledger, nil)
}

// startListenerWithRoutes is the full form, for tests that care about routing.
func startListenerWithRoutes(t *testing.T, r Resolver, ledger Ledger, routes Routes) string {
	t.Helper()
	return startListenerFull(t, r, ledger, routes, nil)
}

// startListenerFull is the form for tests that care about transfers.
func startListenerFull(t *testing.T, r Resolver, ledger Ledger, routes Routes, probe ChannelProbe) string {
	return startListenerLive(t, r, ledger, routes, probe, nil)
}

// startListenerLive is the form for tests that care about the leg that comes
// BACK from a transfer, which needs to find the call it continues.
func startListenerLive(t *testing.T, r Resolver, ledger Ledger, routes Routes, probe ChannelProbe, live LiveCalls) string {
	t.Helper()

	if ledger == nil {
		ledger = &fakeLedger{}
	}
	if routes == nil {
		routes = &fakeRoutes{}
	}
	if probe == nil {
		// Reports every channel gone, so an ordinary call finalises the way it
		// always did.
		probe = &fakeProbe{}
	}

	// Port 0 so tests can run in parallel and on a machine where 8022 is busy.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	l := New(Config{
		Addr:          addr,
		Keepalive:     time.Minute,
		Node:          "test",
		RecordingPath: "/mnt/recordings",
		// Low, so the suite is not mostly sleeping. The behaviour under test
		// is which event wins, not how long the wait is.
		HangupWait:   150 * time.Millisecond,
		TransferPoll: 50 * time.Millisecond,
	}, r, ledger, routes, probe, live, quiet())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("listener did not stop within 5s")
		}
	})

	waitForListener(t, addr)
	return addr
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("listener never came up on %s", addr)
}

// ---------------------------------------------------------------------------
// The handshake
// ---------------------------------------------------------------------------

// FreeSWITCH says nothing when it connects; it waits for `connect`. Getting
// this backwards deadlocks every call.
func TestHandshake_ControllerSendsConnectFirst(t *testing.T) {
	r := &stubResolver{verdict: refused(admission.ReasonUnknownDID)}
	fs := dialFakeFS(t, startListener(t, r, nil), defaultChannel())

	// dialFakeFS asserts `connect` arrives first. Next must be the events
	// subscription.
	fs.expectCommand("myevents")
	fs.replyOK("Events Enabled")
}

// Without myevents the connection gets the channel data and then silence —
// no answer, no hangup, ever. The call would connect, this service would never
// learn it ended, and the socket would hold a call nobody is on.
func TestHandshake_SubscribesToEvents(t *testing.T) {
	r := &stubResolver{verdict: refused(admission.ReasonUnknownDID)}
	fs := dialFakeFS(t, startListener(t, r, nil), defaultChannel())

	cmd := fs.expectCommand("myevents")
	fs.replyOK("Events Enabled")

	ch := defaultChannel()
	if !strings.Contains(cmd, ch.uuid) {
		t.Errorf("myevents must name this channel's UUID, got %q", cmd)
	}
}

// If the subscription is refused the call must be abandoned, not carried on
// with. Continuing would mean a call this service can never observe the end of.
func TestHandshake_AbandonsCallIfSubscribeFails(t *testing.T) {
	r := &stubResolver{verdict: admitted()}
	fs := dialFakeFS(t, startListener(t, r, nil), defaultChannel())

	fs.expectCommand("myevents")
	fs.replyError("Subscription failed")

	if r.calls != 0 {
		t.Errorf("admission should not run once the subscription failed, got %d calls", r.calls)
	}
}

// ---------------------------------------------------------------------------
// Channel data
// ---------------------------------------------------------------------------

// The dialled number reaches admission as FreeSWITCH sent it, and normalising
// is admission's job — not something done twice in two places.
func TestChannelData_DialledNumberReachesAdmission(t *testing.T) {
	r := &stubResolver{verdict: refused(admission.ReasonUnknownDID)}
	fs := dialFakeFS(t, startListener(t, r, nil), defaultChannel())

	fs.expectCommand("myevents")
	fs.replyOK("Events Enabled")
	fs.expectCommand("linger")
	fs.replyOK("")

	fs.expectCommand("sendmsg") // the hangup
	fs.replyOK("")

	if r.lastDID != defaultChannel().destination {
		t.Errorf("admission saw did %q, want %q", r.lastDID, defaultChannel().destination)
	}
}

// Without a UUID no command can be addressed to the channel, so there is
// nothing to do but drop the socket — including no way to hang up politely.
func TestChannelData_NoUUIDDropsTheSocket(t *testing.T) {
	r := &stubResolver{verdict: admitted()}

	ch := defaultChannel()
	ch.uuid = ""
	fs := dialFakeFS(t, startListener(t, r, nil), ch)

	if err := expectClosed(fs, 3*time.Second); err != nil {
		t.Errorf("want the socket dropped: %v", err)
	}
	if r.calls != 0 {
		t.Errorf("admission should not run without a channel UUID, got %d calls", r.calls)
	}
}

// ---------------------------------------------------------------------------
// Hangup causes — the only thing the carrier and the CDR ever see
// ---------------------------------------------------------------------------

func TestHangupCause(t *testing.T) {
	cases := []struct {
		name    string
		verdict admission.Verdict
		err     error
		want    string
	}{
		{
			// Not ours. 404, and it is what stops a carrier retrying.
			name:    "unknown number",
			verdict: refused(admission.ReasonUnknownDID),
			want:    causeUnallocated,
		},
		{
			name:    "withdrawn number",
			verdict: refused(admission.ReasonDIDInactive),
			want:    causeUnallocated,
		},
		{
			// Ours, refused on purpose. 403 not 404 — telling a carrier the
			// number does not exist invites it to be reallocated.
			name:    "suspended tenant",
			verdict: refused(admission.ReasonTenantSuspended),
			want:    causeRejected,
		},
		{
			name:    "plan switched off",
			verdict: refused(admission.ReasonPlanInactive),
			want:    causeRejected,
		},
		{
			name:    "plan configured to refuse",
			verdict: refused(admission.ReasonPlanRejects),
			want:    causeRejected,
		},
		{
			// Our misconfiguration, not the caller's problem. The number does
			// exist and will work once someone finishes setting it up.
			name:    "provisioned with no plan",
			verdict: refused(admission.ReasonNoPlan),
			want:    causeTemporary,
		},
		{
			name:    "at capacity",
			verdict: refused(admission.ReasonOverCapacity),
			want:    causeCongestion,
		},
		{
			// An outage must not be reported as a number that does not exist.
			name: "database unreachable",
			err:  errors.New("connection refused"),
			want: causeTemporary,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &stubResolver{verdict: tc.verdict, err: tc.err}
			fs := dialFakeFS(t, startListener(t, r, nil), defaultChannel())

			fs.expectCommand("myevents")
			fs.replyOK("Events Enabled")
			fs.expectCommand("linger")
			fs.replyOK("")

			cmd := fs.expectCommand("sendmsg")
			fs.replyOK("")

			if !strings.Contains(cmd, "call-command: hangup") {
				t.Fatalf("want a hangup, got %q", cmd)
			}
			if !strings.Contains(cmd, "hangup-cause: "+tc.want) {
				t.Errorf("want cause %q, got %q", tc.want, cmd)
			}
		})
	}
}

// Every refusal must carry a cause. A bare hangup reads as normal clearing,
// which makes a refused call indistinguishable from one that simply ended.
func TestHangup_AlwaysCarriesACause(t *testing.T) {
	r := &stubResolver{verdict: refused(admission.ReasonUnknownDID)}
	fs := dialFakeFS(t, startListener(t, r, nil), defaultChannel())

	fs.expectCommand("myevents")
	fs.replyOK("Events Enabled")
	fs.expectCommand("linger")
	fs.replyOK("")

	cmd := fs.expectCommand("sendmsg")
	fs.replyOK("")

	if !strings.Contains(cmd, "hangup-cause:") {
		t.Fatalf("hangup with no cause: %q", cmd)
	}
	if strings.Contains(cmd, "NORMAL_CLEARING") {
		t.Error("NORMAL_CLEARING makes a refusal look like an ordinary end of call")
	}
}

// ---------------------------------------------------------------------------
// Pre-answer
// ---------------------------------------------------------------------------

// The dialplan hands the call over deliberately unanswered. Answering starts
// carrier billing on a call that may be unroutable, and makes a rejection look
// to the caller like a call that connected and then died.
func TestRefusal_HappensBeforeAnswer(t *testing.T) {
	r := &stubResolver{verdict: refused(admission.ReasonUnknownDID)}
	fs := dialFakeFS(t, startListener(t, r, nil), defaultChannel())

	fs.expectCommand("myevents")
	fs.replyOK("Events Enabled")
	fs.expectCommand("linger")
	fs.replyOK("")
	fs.expectCommand("sendmsg")
	fs.replyOK("")

	for _, cmd := range fs.commands() {
		if strings.Contains(cmd, "execute-app-name: answer") {
			t.Fatal("the call was answered before being refused")
		}
	}
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// An event arrives as a message whose body is itself a header block. A reply
// arriving while events are flowing must still be matched to its command —
// mixing the two up is how a command appears to time out under load.
func TestEvents_DoNotConsumeCommandReplies(t *testing.T) {
	r := &stubResolver{verdict: refused(admission.ReasonUnknownDID)}
	ch := defaultChannel()
	fs := dialFakeFS(t, startListener(t, r, nil), ch)

	fs.expectCommand("myevents")

	// An event lands between the command and its reply, which is exactly what
	// happens on a busy channel.
	fs.sendEvent("CHANNEL_STATE", ch.uuid)
	fs.replyOK("Events Enabled")

	fs.expectCommand("linger")
	fs.sendEvent("CHANNEL_EXECUTE", ch.uuid)
	fs.replyOK("")

	cmd := fs.expectCommand("sendmsg")
	fs.replyOK("")

	if !strings.Contains(cmd, "call-command: hangup") {
		t.Errorf("the call did not reach hangup with events interleaved: %q", cmd)
	}
}

// ---------------------------------------------------------------------------
// Shutdown
// ---------------------------------------------------------------------------

// Stopping ends the calls this node is carrying — there is no drain. What the
// grace period buys is ending them cleanly rather than the process vanishing
// mid-write, and Run must actually wait.
func TestRun_StopsAcceptingAndWaits(t *testing.T) {
	r := &stubResolver{verdict: refused(admission.ReasonUnknownDID)}

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	l := New(Config{Addr: addr, Keepalive: time.Minute}, r, &fakeLedger{}, &fakeRoutes{}, &fakeProbe{}, nil, quiet())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	waitForListener(t, addr)

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned an error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}

	// The port must be free: a listener still holding it would make the next
	// start fail with "address already in use" and look like a stale process.
	if c, err := net.DialTimeout("tcp", addr, 250*time.Millisecond); err == nil {
		_ = c.Close()
		t.Error("the listener is still accepting after shutdown")
	}
}

// expectClosed waits for the peer to close the connection.
func expectClosed(fs *fakeFS, within time.Duration) error {
	_ = fs.conn.SetReadDeadline(time.Now().Add(within))

	buf := make([]byte, 1)
	_, err := fs.conn.Read(buf)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("connection still open")
	}
	return err
}

// fakeRoutes stands in for the routing config in the database.
type fakeRoutes struct {
	agent      route.AgentConfig
	agentErr   error
	targets    []route.HandoffTarget
	targetsErr error
	plan       route.Plan
	planErr    error
}

func (f *fakeRoutes) AgentConfig(context.Context, string) (route.AgentConfig, error) {
	if f.agentErr != nil {
		return route.AgentConfig{}, f.agentErr
	}
	return f.agent, nil
}

func (f *fakeRoutes) PlanByID(context.Context, string) (route.Plan, error) {
	if f.planErr != nil {
		return route.Plan{}, f.planErr
	}
	return f.plan, nil
}

func (f *fakeRoutes) HandoffTargets(context.Context, string) ([]route.HandoffTarget, error) {
	if f.targetsErr != nil {
		return nil, f.targetsErr
	}
	return f.targets, nil
}

// fakeProbe stands in for asking FreeSWITCH whether a channel is still up.
type fakeProbe struct {
	mu sync.Mutex

	// alive is how many more times ChannelExists reports the channel up. It
	// counts down, so a test can model "transferred, then ended".
	alive int
	err   error
	calls int
}

func (f *fakeProbe) ChannelExists(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	if f.err != nil {
		return false, f.err
	}
	if f.alive > 0 {
		f.alive--
		return true, nil
	}
	return false, nil
}

func (f *fakeProbe) probes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeLive stands in for the calls table during a transfer.
type fakeLive struct {
	mu       sync.Mutex
	call     calls.Live
	err      error
	lookedUp []string
}

func (f *fakeLive) LiveCall(_ context.Context, uuid string) (calls.Live, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookedUp = append(f.lookedUp, uuid)
	if f.err != nil {
		return calls.Live{}, f.err
	}
	return f.call, nil
}

func (f *fakeLive) lookups() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lookedUp...)
}
