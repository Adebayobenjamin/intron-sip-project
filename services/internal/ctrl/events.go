package ctrl

import (
	"sync"
	"time"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/esl"
)

// FreeSWITCH event names this service acts on.
const (
	eventHangup          = "CHANNEL_HANGUP"
	eventHangupComplete  = "CHANNEL_HANGUP_COMPLETE"
	eventAnswer          = "CHANNEL_ANSWER"
	eventExecuteComplete = "CHANNEL_EXECUTE_COMPLETE"
)

// completeWait is how long to wait for CHANNEL_HANGUP_COMPLETE after the call
// ends before settling for CHANNEL_HANGUP.
//
// The two are not interchangeable. CHANNEL_HANGUP fires the moment the channel
// starts tearing down; CHANNEL_HANGUP_COMPLETE fires once FreeSWITCH has
// finished, and it is the one carrying the populated CDR variables —
// billsec, the SIP disposition, the final cause. Taking the first event
// available means reading fields that are not filled in yet and recording
// blanks.
//
// Two seconds is generous for an event that normally follows within
// milliseconds. The socket stays open long enough to receive it because the
// call sets `linger`; without that the connection can close in the same
// instant the channel does and the event is never delivered at all.
const completeWait = 2 * time.Second

// hangupInfo is what FreeSWITCH says about how a call ended.
type hangupInfo struct {
	// Cause is FreeSWITCH's hangup cause, e.g. NORMAL_CLEARING, USER_BUSY.
	Cause string

	// Disposition is variable_sip_hangup_disposition: recv_bye when the far
	// end of this channel hung up, send_bye when FreeSWITCH did.
	Disposition string

	// Complete reports that this came from CHANNEL_HANGUP_COMPLETE rather
	// than the earlier, less populated CHANNEL_HANGUP.
	Complete bool

	// Seen is false when no hangup event arrived at all — a socket that died
	// before the call ended, or a linger too short to deliver it.
	Seen bool
}

// hangupWatcher collects hangup events off the channel's event stream, and
// tracks which applications have finished running on it.
//
// Writes come from the read-loop goroutine and reads from the call goroutine,
// so everything is behind a mutex.
type hangupWatcher struct {
	mu   sync.Mutex
	info hangupInfo

	completed chan struct{}
	once      sync.Once

	answered bool

	// apps holds one channel per application being waited on, keyed by the
	// Event-UUID it was sent with. Closed when its CHANNEL_EXECUTE_COMPLETE
	// arrives.
	apps map[string]chan struct{}
}

func newHangupWatcher() *hangupWatcher {
	return &hangupWatcher{
		completed: make(chan struct{}),
		apps:      make(map[string]chan struct{}),
	}
}

// expectApp registers interest in an application's completion and returns the
// channel that will close when it finishes.
//
// MUST BE CALLED BEFORE THE COMMAND IS SENT. FreeSWITCH can finish a short
// application and deliver its event before the sending goroutine runs again,
// and a registration made afterwards would wait for something that has already
// happened.
func (w *hangupWatcher) expectApp(id string) <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()

	if ch, ok := w.apps[id]; ok {
		return ch
	}

	ch := make(chan struct{})
	w.apps[id] = ch
	return ch
}

// hungUp closes when CHANNEL_HANGUP_COMPLETE arrives.
//
// Waiting on an application alone is not enough: if the channel dies without
// that application reporting completion — a socket lost mid-call, a
// FreeSWITCH restart — the waiter would block for the process's lifetime
// holding a concurrency slot.
func (w *hangupWatcher) hungUp() <-chan struct{} {
	return w.completed
}

// observe records a hangup event. Later, more complete information wins;
// CHANNEL_HANGUP_COMPLETE never loses to CHANNEL_HANGUP.
func (w *hangupWatcher) observe(ev *esl.Message) {
	name := esl.EventName(ev)

	w.mu.Lock()
	defer w.mu.Unlock()

	switch name {
	case eventAnswer:
		w.answered = true
		return

	case eventExecuteComplete:
		// Application-UUID is the Event-UUID the command went out with. An
		// event for an application nobody is waiting on is normal and ignored:
		// every application the channel runs reports completion here, not just
		// the ones this service asked about.
		if id := ev.Get("Application-UUID"); id != "" {
			if done, ok := w.apps[id]; ok {
				delete(w.apps, id)
				close(done)
			}
		}
		return

	case eventHangup, eventHangupComplete:
	default:
		return
	}

	complete := name == eventHangupComplete

	// A second CHANNEL_HANGUP must not overwrite a CHANNEL_HANGUP_COMPLETE
	// already recorded, and neither must an out-of-order delivery.
	if w.info.Complete && !complete {
		return
	}

	w.info = hangupInfo{
		Cause:       ev.GetAny("Hangup-Cause", "variable_hangup_cause"),
		Disposition: ev.Get("variable_sip_hangup_disposition"),
		Complete:    complete,
		Seen:        true,
	}

	if complete {
		w.once.Do(func() { close(w.completed) })
	}
}

// wait blocks for CHANNEL_HANGUP_COMPLETE, up to d, and returns the best
// information available — which may be from CHANNEL_HANGUP, or nothing at all.
func (w *hangupWatcher) wait(d time.Duration) hangupInfo {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-w.completed:
	case <-timer.C:
		// Settle for CHANNEL_HANGUP. Worse data is better than none: the
		// cause is usually right even on the early event, and the alternative
		// is a record with no cause at all.
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	return w.info
}

// apply writes what FreeSWITCH reported onto the record.
//
// It does NOT overwrite a hangup this service issued. When the platform
// refuses a call it already knows the cause and that it was responsible, and
// FreeSWITCH echoing that back as NORMAL_CLEARING would erase the reason the
// call was refused — which is the one field anyone asks about afterwards.
func (info hangupInfo) apply(rec *calls.Record, platformHungUp bool) {
	if !info.Seen || platformHungUp {
		return
	}

	if info.Cause != "" {
		rec.HangupCause = info.Cause
	}

	rec.HangupBy = info.hangupBy()
}

// hangupBy reads who ended the call from the SIP disposition.
//
// This is observed on the CALLER's channel, which is what makes the mapping
// work:
//
//	recv_bye   FreeSWITCH received a BYE on this channel   the caller hung up
//	send_bye   FreeSWITCH sent a BYE on this channel       something else ended
//	           the call and hangup_after_bridge tore this
//	           leg down — in practice the far end
//
// An unknown or empty disposition returns "" rather than a guess. A wrong
// attribution is worse than a missing one: nobody audits a blank field, and
// everybody trusts a populated one.
func (info hangupInfo) hangupBy() string {
	switch info.Disposition {
	case "recv_bye":
		return calls.HangupByCaller
	case "send_bye":
		return calls.HangupByCallee
	default:
		return ""
	}
}
