package ctrl

import (
	"testing"
	"time"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/esl"
)

// playCallWithHangup runs a call and delivers hangup events after the bridge,
// the way a real switch does when the far end ends the call.
func playCallWithHangup(t *testing.T, r Resolver, l Ledger, routes Routes, cause, disposition string) []string {
	t.Helper()

	fs := dialFakeFS(t, startListenerWithRoutes(t, r, l, routes), defaultChannel())
	return fs.serveCallThenHangup(defaultChannel().uuid, cause, disposition)
}

// ---------------------------------------------------------------------------
// Who hung up
// ---------------------------------------------------------------------------

// Observed, not guessed. The bridge returning only says the call ended, and
// attributing that to the callee every time is wrong about half the time.
func TestHangup_WhoEndedTheCallIsObserved(t *testing.T) {
	cases := []struct {
		name        string
		disposition string
		want        string
	}{
		{
			// FreeSWITCH received a BYE on the caller's channel.
			name:        "caller hung up",
			disposition: "recv_bye",
			want:        calls.HangupByCaller,
		},
		{
			// FreeSWITCH sent the BYE, because the far end ended and
			// hangup_after_bridge tore this leg down.
			name:        "far end hung up",
			disposition: "send_bye",
			want:        calls.HangupByCallee,
		},
		{
			// A wrong attribution is worse than a missing one: nobody audits a
			// blank field and everybody trusts a populated one.
			name:        "unknown disposition",
			disposition: "",
			want:        "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := &fakeLedger{}
			playCallWithHangup(t, &stubResolver{verdict: aiPlan()}, ledger,
				&fakeRoutes{}, "NORMAL_CLEARING", tc.disposition)

			rec := ledger.waitForRecord(t)
			if rec.HangupBy != tc.want {
				t.Errorf("HangupBy = %q, want %q", rec.HangupBy, tc.want)
			}
		})
	}
}

// The cause FreeSWITCH reports beats anything this service inferred, because
// it is the one the carrier actually saw.
func TestHangup_CauseComesFromTheEvent(t *testing.T) {
	ledger := &fakeLedger{}
	playCallWithHangup(t, &stubResolver{verdict: aiPlan()}, ledger,
		&fakeRoutes{}, "USER_BUSY", "send_bye")

	rec := ledger.waitForRecord(t)
	if rec.HangupCause != "USER_BUSY" {
		t.Errorf("HangupCause = %q, want USER_BUSY", rec.HangupCause)
	}
}

// A hangup this service issued keeps its own cause. FreeSWITCH echoing back
// NORMAL_CLEARING would erase the reason a call was refused, which is the one
// field anyone asks about afterwards.
func TestHangup_PlatformRefusalKeepsItsOwnCause(t *testing.T) {
	ledger := &fakeLedger{}

	fs := dialFakeFS(t,
		startListenerWithRoutes(t,
			&stubResolver{verdict: refused(admission.ReasonTenantSuspended)},
			ledger, &fakeRoutes{}),
		defaultChannel())

	// The switch confirms the hangup and reports it as ordinary clearing,
	// which is exactly what it does in reality.
	go func() {
		time.Sleep(150 * time.Millisecond)
		fs.sendHangupEvent(eventHangupComplete, defaultChannel().uuid,
			"NORMAL_CLEARING", "send_bye")
	}()

	fs.serveUntilClose()

	rec := ledger.waitForRecord(t)
	if rec.HangupCause != causeRejected {
		t.Errorf("HangupCause = %q, want %q — the refusal reason was overwritten",
			rec.HangupCause, causeRejected)
	}
	if rec.HangupBy != calls.HangupByPlatform {
		t.Errorf("HangupBy = %q, want %q", rec.HangupBy, calls.HangupByPlatform)
	}
}

// ---------------------------------------------------------------------------
// Which event wins
// ---------------------------------------------------------------------------

// CHANNEL_HANGUP fires first and carries less; CHANNEL_HANGUP_COMPLETE carries
// the populated CDR variables. Taking the first one available means recording
// fields that are not filled in yet.
func TestHangupWatcher_CompletePreferredOverEarlyEvent(t *testing.T) {
	w := newHangupWatcher()

	w.observe(eventMsg(t, eventHangup, "NORMAL_CLEARING", ""))
	w.observe(eventMsg(t, eventHangupComplete, "NORMAL_CLEARING", "recv_bye"))

	info := w.wait(time.Second)

	if !info.Complete {
		t.Error("want the information from CHANNEL_HANGUP_COMPLETE")
	}
	if info.Disposition != "recv_bye" {
		t.Errorf("Disposition = %q, want recv_bye", info.Disposition)
	}
}

// Out-of-order or duplicate delivery must not downgrade what is already known.
func TestHangupWatcher_EarlyEventCannotOverwriteComplete(t *testing.T) {
	w := newHangupWatcher()

	w.observe(eventMsg(t, eventHangupComplete, "USER_BUSY", "recv_bye"))
	w.observe(eventMsg(t, eventHangup, "NORMAL_CLEARING", ""))

	info := w.wait(time.Second)

	if info.Cause != "USER_BUSY" || info.Disposition != "recv_bye" {
		t.Errorf("the complete event was overwritten: %+v", info)
	}
}

// If CHANNEL_HANGUP_COMPLETE never arrives — a socket that died, a linger too
// short — settle for what CHANNEL_HANGUP gave. Worse data beats none.
func TestHangupWatcher_FallsBackToTheEarlyEvent(t *testing.T) {
	w := newHangupWatcher()
	w.observe(eventMsg(t, eventHangup, "USER_BUSY", ""))

	start := time.Now()
	info := w.wait(200 * time.Millisecond)

	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("returned after %s; it should wait for the complete event", elapsed)
	}
	if !info.Seen || info.Cause != "USER_BUSY" {
		t.Errorf("want the early event's cause, got %+v", info)
	}
	if info.Complete {
		t.Error("Complete should be false when only CHANNEL_HANGUP arrived")
	}
}

// No hangup event at all must leave the record untouched rather than blanking
// a cause this service already knows.
func TestHangupWatcher_NoEventChangesNothing(t *testing.T) {
	w := newHangupWatcher()
	info := w.wait(50 * time.Millisecond)

	if info.Seen {
		t.Error("want Seen false when no event arrived")
	}

	rec := calls.Record{HangupCause: causeTemporary, HangupBy: calls.HangupByPlatform}
	info.apply(&rec, false)

	if rec.HangupCause != causeTemporary {
		t.Errorf("HangupCause was overwritten to %q", rec.HangupCause)
	}
}

// Waiting returns as soon as the complete event lands, rather than burning the
// full timeout on every call.
func TestHangupWatcher_ReturnsImmediatelyOnComplete(t *testing.T) {
	w := newHangupWatcher()

	go func() {
		time.Sleep(20 * time.Millisecond)
		w.observe(eventMsg(t, eventHangupComplete, "NORMAL_CLEARING", "recv_bye"))
	}()

	start := time.Now()
	w.wait(3 * time.Second)

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %s after the event arrived", elapsed)
	}
}

// eventMsg builds a parsed event body, the shape observe expects.
func eventMsg(t *testing.T, name, cause, disposition string) *esl.Message {
	t.Helper()

	body := "Event-Name: " + name + "\nHangup-Cause: " + cause + "\n"
	if disposition != "" {
		body += "variable_sip_hangup_disposition: " + disposition + "\n"
	}
	body += "\n"

	m, err := esl.ParseEventBody([]byte(body))
	if err != nil {
		t.Fatalf("building event: %v", err)
	}
	return m
}
