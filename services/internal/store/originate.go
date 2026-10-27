package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
)

// QueueOriginate records a request for an outbound call.
//
// The row is the handoff to the controller. Written by the control plane,
// which cannot dial: only the controller holds ESL connections.
func (p *Pool) QueueOriginate(ctx context.Context, r calls.OriginateRequest) (calls.OriginateRequest, error) {
	err := p.QueryRow(ctx, `
		INSERT INTO originate_requests (
			call_uuid, tenant_id, traffic_config_id, from_did, to_number, trunk_id
		) VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, call_uuid, tenant_id, COALESCE(traffic_config_id, 0), from_did,
		          to_number, COALESCE(trunk_id, 0), status, created_at, updated_at`,
		r.CallUUID, r.TenantID, nullInt64Arg(r.TrafficConfigID),
		r.FromDID, r.ToNumber, nullInt64Arg(r.TrunkID),
	).Scan(&r.ID, &r.CallUUID, &r.TenantID, &r.TrafficConfigID, &r.FromDID,
		&r.ToNumber, &r.TrunkID, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return calls.OriginateRequest{}, fmt.Errorf("store: queueing originate: %w", err)
	}
	return r, nil
}

// ClaimOriginate takes the oldest pending request, or reports there is none.
//
// # FOR UPDATE SKIP LOCKED is the whole mechanism
//
// It locks the row it picks and SKIPS rows another transaction already holds.
// Two controllers polling in the same instant therefore get different rows —
// never the same one twice, and neither waits on the other. No leader
// election, no lock service, no coordination: Postgres arbitrates.
//
// Without SKIP LOCKED the second poller would BLOCK on the first's row and
// then claim it after the commit — placing the same call twice, which for an
// outbound call means ringing a customer twice and billing for both.
//
// The subquery matters too. Locking inside a plain UPDATE ... WHERE status =
// 'pending' would lock every pending row, not one, and serialise the whole
// queue behind a single worker.
//
// The trunk host is joined in here rather than fetched separately: the
// controller needs it to build the dial string, and a second query would let
// the trunk change between the two.
func (p *Pool) ClaimOriginate(ctx context.Context, claimedBy string) (calls.OriginateRequest, bool, error) {
	var r calls.OriginateRequest

	err := p.QueryRow(ctx, `
		WITH claimed AS (
			UPDATE originate_requests
			   SET status     = 'claimed',
			       claimed_by = $1,
			       claimed_at = now(),
			       updated_at = now()
			 WHERE id = (
				   SELECT id FROM originate_requests
				    WHERE status = 'pending'
				    ORDER BY created_at
				    FOR UPDATE SKIP LOCKED
				    LIMIT 1)
			RETURNING *
		)
		SELECT c.id, c.call_uuid, c.tenant_id, COALESCE(c.traffic_config_id, 0),
		       c.from_did, c.to_number, COALESCE(c.trunk_id, 0),
		       COALESCE(t.sip_host, ''), COALESCE(t.sip_port, 0),
		       c.status, c.claimed_by, c.claimed_at, c.created_at, c.updated_at
		  FROM claimed c
		  LEFT JOIN trunks t ON t.id = c.trunk_id AND t.deleted_at IS NULL`,
		claimedBy,
	).Scan(&r.ID, &r.CallUUID, &r.TenantID, &r.TrafficConfigID, &r.FromDID,
		&r.ToNumber, &r.TrunkID, &r.TrunkHost, &r.TrunkPort,
		&r.Status, &r.ClaimedBy, &r.ClaimedAt, &r.CreatedAt, &r.UpdatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		// Nothing to do. The ordinary case, hundreds of times a minute.
		return calls.OriginateRequest{}, false, nil
	}
	if err != nil {
		return calls.OriginateRequest{}, false, fmt.Errorf("store: claiming originate: %w", err)
	}

	return r, true, nil
}

// MarkOriginatePlaced records that FreeSWITCH accepted the command.
//
// Placed means DIALLED, not answered. What happens after — ringing, busy, no
// answer — arrives as events on the socket FreeSWITCH opens when the far end
// picks up, and ends in call_history like any other call.
func (p *Pool) MarkOriginatePlaced(ctx context.Context, callUUID string) error {
	return p.markOriginate(ctx, callUUID, calls.OriginatePlaced, "")
}

// MarkOriginateFailed records why a request could not be dialled.
//
// The reason is stored rather than only logged: "why did that call never
// happen" is asked days later, by someone who cannot read the controller's
// journal.
func (p *Pool) MarkOriginateFailed(ctx context.Context, callUUID, reason string) error {
	return p.markOriginate(ctx, callUUID, calls.OriginateFailed, reason)
}

func (p *Pool) markOriginate(ctx context.Context, callUUID, status, reason string) error {
	tag, err := p.Exec(ctx, `
		UPDATE originate_requests
		   SET status = $2, error = $3, updated_at = now()
		 WHERE call_uuid = $1`,
		callUUID, status, nullStringArg(reason))
	if err != nil {
		return fmt.Errorf("store: marking originate %s: %w", status, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetOriginate reads one request, for the status endpoint.
func (p *Pool) GetOriginate(ctx context.Context, callUUID, tenantID string) (calls.OriginateRequest, error) {
	var r calls.OriginateRequest

	err := p.QueryRow(ctx, `
		SELECT id, call_uuid, tenant_id, COALESCE(traffic_config_id, 0), from_did, to_number,
		       COALESCE(trunk_id, 0), status, COALESCE(claimed_by, ''), claimed_at,
		       COALESCE(error, ''), created_at, updated_at
		  FROM originate_requests
		 WHERE call_uuid = $1 AND ($2::text IS NULL OR tenant_id = $2)`,
		callUUID, nullStringArg(tenantID),
	).Scan(&r.ID, &r.CallUUID, &r.TenantID, &r.TrafficConfigID, &r.FromDID, &r.ToNumber,
		&r.TrunkID, &r.Status, &r.ClaimedBy, &r.ClaimedAt, &r.Error,
		&r.CreatedAt, &r.UpdatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return calls.OriginateRequest{}, ErrNotFound
	}
	if err != nil {
		return calls.OriginateRequest{}, fmt.Errorf("store: getting originate: %w", err)
	}
	return r, nil
}
