// Package calls owns live call state and call records.
//
// Two tables, and the split is deliberate. `calls` holds only calls in
// progress and is the concurrency ledger — a row exists exactly as long as the
// call does. `call_history` is the permanent record and is written once, at
// hangup, in the same transaction that releases the slot.
//
// The controller writes both directly. An earlier design routed them through
// the control-plane API because the controller had no database credential; it
// has one now, and a hangup write that would have needed an outbox, a retry
// policy and an idempotency key to survive a restart is instead one BEGIN.
package calls

import (
	"errors"
	"fmt"
	"time"
)

// Direction of a call.
const (
	DirectionInbound  = "inbound"
	DirectionOutbound = "outbound"
)

// States a live call moves through.
const (
	StateRinging  = "ringing"
	StateAnswered = "answered"
	StateBridged  = "bridged"
	StateHandoff  = "handoff"
)

// Who ended the call, for the record.
const (
	HangupByCaller = "caller"
	HangupByCallee = "callee"

	// HangupByPlatform is every call this platform ended itself — a refusal,
	// a capacity limit, a failure. Separating it from a caller hangup is the
	// difference between answering a support question and guessing at it.
	HangupByPlatform = "platform"
)

// ErrAtCapacity means the tenant is already at its concurrency cap.
//
// A distinct error rather than a boolean, because the caller has to respond
// differently: congestion tells a carrier to retry, and a refusal tells it not
// to.
var ErrAtCapacity = errors.New("calls: tenant is at its concurrency cap")

// ErrNoLiveCall means no call is currently in progress under that UUID.
//
// Ordinary, not a fault. A transferred leg arriving after its own call has
// already been finalised looks exactly like this, and so does anything dialling
// the reserved transfer word without having been transferred.
var ErrNoLiveCall = errors.New("calls: no call in progress with that uuid")

// Claim is the request to take a concurrency slot.
type Claim struct {
	UUID      string
	TenantID  string
	PlanID    string
	TrunkID   int64
	Direction string
	DID       string
	Caller    string
	Callee    string
	Node      string
}

// Record is a finished call, written to call_history.
//
// The fields are generous because a CDR column cannot be backfilled after the
// calls have happened — whatever is missing here is missing forever.
type Record struct {
	UUID      string
	SIPCallID string

	TenantID  string
	PlanID    string
	TrunkID   int64
	Direction string

	DID    string
	Caller string
	Callee string

	HandoffTargetID int64
	LiveKitRoom     string
	Node            string

	InviteAt time.Time
	RingAt   time.Time
	AnswerAt time.Time
	EndAt    time.Time

	SIPCode     int16
	HangupCause string
	HangupBy    string

	// Outcome is the admission reason for a refused call, and how the call
	// ended for one that ran. It is what someone filters on when asked why a
	// number was not working last Tuesday.
	Outcome string

	RecordingURL string
}

// Duration helpers. Zero times mean the stage never happened — a call that was
// refused has no answer time — and must produce zero, not a duration measured
// from the zero instant.
func (r Record) ringSeconds() int32 {
	if r.RingAt.IsZero() {
		return 0
	}
	end := r.AnswerAt
	if end.IsZero() {
		end = r.EndAt
	}
	if end.IsZero() {
		return 0
	}
	return seconds(end.Sub(r.RingAt))
}

func (r Record) talkSeconds() int32 {
	if r.AnswerAt.IsZero() || r.EndAt.IsZero() {
		return 0
	}
	return seconds(r.EndAt.Sub(r.AnswerAt))
}

func (r Record) totalSeconds() int32 {
	if r.InviteAt.IsZero() || r.EndAt.IsZero() {
		return 0
	}
	return seconds(r.EndAt.Sub(r.InviteAt))
}

func seconds(d time.Duration) int32 {
	if d < 0 {
		return 0
	}
	return int32(d / time.Second)
}

// Validate refuses a record that could be written and then never found.
func (r Record) Validate() error {
	switch {
	case r.UUID == "":
		return fmt.Errorf("calls: record has no call UUID")
	case r.TenantID == "":
		// Every query is scoped by tenant, so a record with none would be
		// written successfully and be invisible to everyone afterwards.
		return fmt.Errorf("calls: record %s has no tenant", r.UUID)
	case r.Direction == "":
		return fmt.Errorf("calls: record %s has no direction", r.UUID)
	}
	return nil
}

// Args returns the record as query arguments, in the column order the insert
// declares. Kept next to the struct so adding a field means touching one file
// rather than discovering at runtime that a column silently went NULL.
//
// Empty strings and zero times become NULL rather than being stored as
// themselves: "never answered" written as the zero instant reads as the year
// 1, and sorts accordingly.
func (r Record) Args() []any {
	return []any{
		r.UUID,
		nullString(r.SIPCallID),
		r.TenantID,
		r.Direction,
		nullString(r.DID),
		nullString(r.Caller),
		nullString(r.Callee),
		nullInt64(r.TrunkID),
		nullString(r.PlanID),
		nullInt64(r.HandoffTargetID),
		nullString(r.LiveKitRoom),
		nullString(r.Node),
		nullTime(r.InviteAt),
		nullTime(r.RingAt),
		nullTime(r.AnswerAt),
		nullTime(r.EndAt),
		r.ringSeconds(),
		r.talkSeconds(),
		r.totalSeconds(),
		nullInt16(r.SIPCode),
		nullString(r.HangupCause),
		nullString(r.HangupBy),
		nullString(r.Outcome),
		nullString(r.RecordingURL),
	}
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullInt16(v int16) any {
	if v == 0 {
		return nil
	}
	return v
}
