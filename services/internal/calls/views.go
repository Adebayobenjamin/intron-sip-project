package calls

import "time"

// Views of call state, for the operator API.
//
// They live here rather than in the handlers because both the store and the
// handlers need them, and a type declared in the handler package would make
// the store import the HTTP layer — which is backwards, and Go says so with an
// import cycle.

// Live is a call in progress.
type Live struct {
	CallUUID        string     `json:"call_uuid"`
	TenantID        string     `json:"tenant_id"`
	TrafficConfigID int64      `json:"traffic_config_id,omitempty"`
	TrunkID         int64      `json:"trunk_id,omitempty"`
	Direction       string     `json:"direction"`
	DID             string     `json:"did,omitempty"`
	Caller          string     `json:"caller,omitempty"`
	State           string     `json:"state"`
	RecordingMode   string     `json:"recording_mode"`
	Node            string     `json:"node,omitempty"`
	LiveKitRoom     string     `json:"livekit_room,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	AnsweredAt      *time.Time `json:"answered_at,omitempty"`
	HeartbeatAt     time.Time  `json:"heartbeat_at"`
}

// Historic is a finished call, as the API returns it.
//
// Deliberately not the same shape as Record. Record is what the controller
// WRITES and carries fields nobody reads back; this is what an operator READS
// and omits empties so a refused call does not come back padded with nulls.
type Historic struct {
	CallUUID        string     `json:"call_uuid"`
	SIPCallID       string     `json:"sip_call_id,omitempty"`
	TenantID        string     `json:"tenant_id"`
	Direction       string     `json:"direction"`
	DID             string     `json:"did,omitempty"`
	Caller          string     `json:"caller,omitempty"`
	Callee          string     `json:"callee,omitempty"`
	TrafficConfigID int64      `json:"traffic_config_id,omitempty"`
	TrunkID         int64      `json:"trunk_id,omitempty"`
	LiveKitRoom     string     `json:"livekit_room,omitempty"`
	RecordingMode   string     `json:"recording_mode,omitempty"`
	InviteAt        *time.Time `json:"invite_at,omitempty"`
	AnswerAt        *time.Time `json:"answer_at,omitempty"`
	EndAt           *time.Time `json:"end_at,omitempty"`
	TalkSeconds     int        `json:"talk_seconds"`
	SIPCode         int        `json:"sip_code,omitempty"`
	HangupCause     string     `json:"hangup_cause,omitempty"`
	HangupBy        string     `json:"hangup_by,omitempty"`
	Outcome         string     `json:"outcome,omitempty"`
	Recording       string     `json:"recording_url,omitempty"`
}
