package calls

import "time"

// Originate statuses.
const (
	OriginatePending = "pending"
	OriginateClaimed = "claimed"
	OriginatePlaced  = "placed"
	OriginateFailed  = "failed"
)

// OriginateRequest is a queued outbound call.
//
// # Why this is a table and not an HTTP call
//
// Only the controller holds ESL connections, so the API cannot dial. Both
// services already share a database, so the handoff is a row: no new port, no
// new credential, and a queued request SURVIVES A CONTROLLER RESTART where an
// in-flight HTTP request would not.
//
// The row is also the audit trail — who asked, when, what happened, and why it
// failed — which an HTTP call would have needed a second mechanism to record.
type OriginateRequest struct {
	ID       int64  `json:"id"`
	CallUUID string `json:"call_uuid"`

	TenantID string `json:"tenant_id"`

	// TrafficConfigID is the OUTBOUND line whose numbers include FromDID. It
	// is found from the number, not named by the caller, and it supplies both
	// the trunk and what the answered customer is connected to.
	TrafficConfigID int64 `json:"traffic_config_id"`

	// FromDID is the number the call presents. One lookup, three answers —
	// ownership, caller ID and trunk; see docs/API.md, Contract 3.
	FromDID  string `json:"from_did"`
	ToNumber string `json:"to_number"`

	TrunkID   int64  `json:"trunk_id,omitempty"`
	TrunkHost string `json:"-"`

	// TrunkPort is the carrier's SIP port. Not every carrier listens on 5060,
	// and a dial string that names only the host sends the INVITE to 5060
	// regardless — which a carrier on another port never answers.
	TrunkPort int `json:"-"`

	Status    string     `json:"status"`
	ClaimedBy string     `json:"claimed_by,omitempty"`
	ClaimedAt *time.Time `json:"claimed_at,omitempty"`
	Error     string     `json:"error,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// OutboundLine is what a from-number resolves to before an outbound call is
// queued: the line that owns it, and the trunk that line leaves by.
type OutboundLine struct {
	TrafficConfigID int64
	TrunkID         int64
	TrunkHost       string
	RecordingMode   string
}
