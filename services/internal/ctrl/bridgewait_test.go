package ctrl

import (
	"strings"
	"testing"
	"time"
)

// recordCount reports how many CDRs have been written so far, without blocking.
func (f *fakeLedger) recordCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

// THE TEST FOR THE BUG THAT REACHED BETA.
//
// `bridge` does not block. The ESL socket is async, so FreeSWITCH's +OK means
// the command was ACCEPTED, not that the application finished. The controller
// treated the reply as the end of the call, waited a couple of seconds for a
// hangup that was not coming yet, and wrote the CDR while the caller was still
// talking:
//
//	08:01:30.445  bridging
//	08:01:32.451  call ended  outcome="bridged"  total_seconds=2   <- ran 30s
//
// Every CDR recorded ~2 seconds, and the concurrency slot was released two
// seconds into every call, so a cap of 3 admitted far more than three.
//
// WHY NO EXISTING TEST CAUGHT IT. The fake switch sent the hangup events
// immediately after acknowledging the bridge, so the call under test ended at
// the same moment it started and the two were indistinguishable. This test
// holds the bridge open instead, which is the only arrangement where the
// difference is visible.
func TestBridge_CallInProgressIsNotRecordedAsFinished(t *testing.T) {
	ledger := &fakeLedger{}
	addr := startListenerWithRoutes(t, &stubResolver{verdict: aiPlan()}, ledger, &fakeRoutes{})

	ch := defaultChannel()
	fs := dialFakeFS(t, addr, ch)

	// Acknowledge everything, but do NOT complete the bridge. This is a call
	// that has connected and is still running.
	bridgeSeen := make(chan string, 1)
	go func() {
		for {
			cmd, err := fs.readOne()
			if err != nil {
				return
			}
			_ = fs.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
			if _, err := fs.conn.Write(
				[]byte("Content-Type: command/reply\r\nReply-Text: +OK\r\n\r\n")); err != nil {
				return
			}
			if strings.Contains(cmd, "execute-app-name: bridge") {
				select {
				case bridgeSeen <- eventUUIDOf(cmd):
				default:
				}
			}
		}
	}()

	var eventID string
	select {
	case eventID = <-bridgeSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("the controller never issued a bridge")
	}

	if eventID == "" {
		t.Fatal("the bridge carried no Event-UUID, so its completion cannot be " +
			"told from any other application's")
	}

	// Well past the hangup wait the old code settled for. A call still in
	// progress must have produced no record and released no slot.
	time.Sleep(600 * time.Millisecond)

	if n := ledger.recordCount(); n != 0 {
		t.Fatalf("wrote %d CDR(s) while the bridge was still up; the call is "+
			"still connected and its duration is not yet known", n)
	}

	// Now end it, the way FreeSWITCH does: the bridge reports completion, then
	// the channel tears down.
	fs.sendExecuteComplete(ch.uuid, "bridge", eventID)
	fs.sendHangupEvent(eventHangup, ch.uuid, "NORMAL_CLEARING", "")
	fs.sendHangupEvent(eventHangupComplete, ch.uuid, "NORMAL_CLEARING", "recv_bye")

	rec := ledger.waitForRecord(t)
	if rec.HangupCause != "NORMAL_CLEARING" {
		t.Errorf("HangupCause = %q, want NORMAL_CLEARING — the hangup arrived "+
			"after the record was written", rec.HangupCause)
	}
	if rec.Outcome != "bridged" {
		t.Errorf("Outcome = %q, want bridged", rec.Outcome)
	}
}

// A completion for a DIFFERENT application must not be mistaken for the
// bridge's. A channel runs several — answer, record_session — and matching on
// the application name alone would end the call at the first one to finish.
func TestBridge_UnrelatedCompletionDoesNotEndTheCall(t *testing.T) {
	ledger := &fakeLedger{}
	addr := startListenerWithRoutes(t, &stubResolver{verdict: aiPlan()}, ledger, &fakeRoutes{})

	ch := defaultChannel()
	fs := dialFakeFS(t, addr, ch)

	bridgeSeen := make(chan string, 1)
	go func() {
		for {
			cmd, err := fs.readOne()
			if err != nil {
				return
			}
			_ = fs.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
			if _, err := fs.conn.Write(
				[]byte("Content-Type: command/reply\r\nReply-Text: +OK\r\n\r\n")); err != nil {
				return
			}
			if strings.Contains(cmd, "execute-app-name: bridge") {
				select {
				case bridgeSeen <- eventUUIDOf(cmd):
				default:
				}
			}
		}
	}()

	var bridgeID string
	select {
	case bridgeID = <-bridgeSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("the controller never issued a bridge")
	}

	// Someone else's completion, and a bridge completion with no id at all.
	fs.sendExecuteComplete(ch.uuid, "playback", "some-other-application-uuid")
	fs.sendExecuteComplete(ch.uuid, "bridge", "")
	time.Sleep(400 * time.Millisecond)

	if n := ledger.recordCount(); n != 0 {
		t.Fatalf("wrote %d CDR(s) on another application's completion; the "+
			"bridge is still running", n)
	}

	// The real one still works.
	fs.sendExecuteComplete(ch.uuid, "bridge", bridgeID)
	fs.sendHangupEvent(eventHangupComplete, ch.uuid, "NORMAL_CLEARING", "recv_bye")
	ledger.waitForRecord(t)
}
