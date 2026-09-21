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
	PlanID   string `json:"plan_id,omitempty"`

	// FromDID is the number the call presents, and the number whose row chose
	// the trunk. One lookup, three answers — see docs/API.md, Contract 3.
	FromDID  string `json:"from_did"`
	ToNumber string `json:"to_number"`

	TrunkID   int64  `json:"trunk_id,omitempty"`
	TrunkHost string `json:"-"`

	Status    string     `json:"status"`
	ClaimedBy string     `json:"claimed_by,omitempty"`
	ClaimedAt *time.Time `json:"claimed_at,omitempty"`
	Error     string     `json:"error,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}
