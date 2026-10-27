package store

import (
	"context"
	"fmt"
	"time"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
)

// LiveCalls returns calls in progress.
func (p *Pool) LiveCalls(ctx context.Context, tenantID string) ([]calls.Live, error) {
	rows, err := p.Query(ctx, `
		SELECT `+liveColumns+`
		  FROM calls
		 WHERE ($1::text IS NULL OR tenant_id = $1)
		 ORDER BY started_at`, nullStringArg(tenantID))
	if err != nil {
		return nil, fmt.Errorf("store: listing live calls: %w", err)
	}
	defer rows.Close()

	out := []calls.Live{}
	for rows.Next() {
		var c calls.Live
		if err := scanLive(rows, &c); err != nil {
			return nil, fmt.Errorf("store: scanning live call: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CallHistory pages through finished calls, newest first.
//
// Keyset pagination on invite_at, not OFFSET. This table grows forever and
// rows arrive constantly, so an offset shifts under a reader between pages —
// which shows up as duplicated and skipped rows in an export nobody
// double-checks. The index is (tenant_id, invite_at DESC), so this is a range
// scan rather than a sort.
func (p *Pool) CallHistory(ctx context.Context, tenantID string, limit int, before *time.Time) ([]calls.Historic, error) {
	rows, err := p.Query(ctx, `
		SELECT call_uuid, COALESCE(sip_call_id, ''), tenant_id, direction,
		       COALESCE(did, ''), COALESCE(caller, ''), COALESCE(callee, ''),
		       COALESCE(traffic_config_id, 0), COALESCE(trunk_id, 0),
		       COALESCE(livekit_room, ''), COALESCE(recording_mode, ''),
		       invite_at, answer_at, end_at,
		       COALESCE(talk_seconds, 0), COALESCE(sip_code, 0),
		       COALESCE(hangup_cause, ''), COALESCE(hangup_by, ''),
		       COALESCE(outcome, ''), COALESCE(recording_url, '')
		  FROM call_history
		 WHERE ($1::text IS NULL OR tenant_id = $1)
		   AND ($2::timestamptz IS NULL OR invite_at < $2)
		 ORDER BY invite_at DESC
		 LIMIT $3`, nullStringArg(tenantID), before, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing call history: %w", err)
	}
	defer rows.Close()

	out := []calls.Historic{}
	for rows.Next() {
		var c calls.Historic
		var code int16
		if err := rows.Scan(&c.CallUUID, &c.SIPCallID, &c.TenantID, &c.Direction,
			&c.DID, &c.Caller, &c.Callee, &c.TrafficConfigID, &c.TrunkID,
			&c.LiveKitRoom, &c.RecordingMode,
			&c.InviteAt, &c.AnswerAt, &c.EndAt,
			&c.TalkSeconds, &code, &c.HangupCause, &c.HangupBy,
			&c.Outcome, &c.Recording); err != nil {
			return nil, fmt.Errorf("store: scanning call record: %w", err)
		}
		c.SIPCode = int(code)
		out = append(out, c)
	}
	return out, rows.Err()
}
