package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
)

// ClaimCall takes a concurrency slot for a call, or reports the tenant is full.
//
// # Why this is a transaction with a lock, and not one clever INSERT
//
// The obvious form is a conditional insert that counts rows in the same
// statement:
//
//	INSERT INTO calls (...) SELECT ...
//	WHERE (SELECT count(*) FROM calls WHERE tenant_id = $2) < cap
//
// That looks atomic and is not. Under READ COMMITTED — the default — the
// subquery cannot see rows inserted by transactions that have not committed
// yet. Two calls arriving in the same instant for a tenant capped at one both
// count zero, both pass the check, and both insert. The cap is exceeded by
// exactly as many calls as arrive concurrently, which is the load at which
// anyone would care.
//
// It is also the kind of bug that never shows up in testing: it needs two
// calls inside the same few milliseconds, so the cap appears to work perfectly
// until the platform is busy enough for it to matter.
//
// So claims for one tenant are serialised with an advisory lock. It is held
// for the length of this transaction only, released automatically on commit or
// rollback, costs no table and no cleanup, and scopes contention to the one
// tenant rather than the whole table.
//
// # Idempotency
//
// The channel UUID is FreeSWITCH's and already unique, so a retried claim for
// a call already claimed refreshes the heartbeat and succeeds. The count
// deliberately excludes this call's own row, or re-claiming a call would
// compare against a total that includes it and report a tenant capped at one
// as full.
func (p *Pool) ClaimCall(ctx context.Context, c calls.Claim) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: beginning claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// hashtext gives a stable int from the tenant id. A collision between two
	// tenants would only make them share a lock — slightly more contention,
	// never a wrong answer.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1))`, c.TenantID); err != nil {
		return fmt.Errorf("store: locking tenant %s: %w", c.TenantID, err)
	}

	var claimed string
	err = tx.QueryRow(ctx, claimCallSQL,
		c.UUID,
		c.TenantID,
		nullStringArg(c.PlanID),
		nullInt64Arg(c.TrunkID),
		c.Direction,
		nullStringArg(c.DID),
		nullStringArg(c.Caller),
		nullStringArg(c.Callee),
		nullStringArg(c.Node),
	).Scan(&claimed)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No row means the WHERE rejected it, and the only condition there is
		// the cap.
		return calls.ErrAtCapacity
	case err != nil:
		return fmt.Errorf("store: claiming call %s: %w", c.UUID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: committing claim: %w", err)
	}
	return nil
}

const claimCallSQL = `
INSERT INTO calls (
    call_uuid, tenant_id, plan_id, trunk_id,
    direction, did, caller, callee, node, state
)
SELECT $1, t.id, $3, $4, $5, $6, $7, $8, $9, 'ringing'
FROM tenants t
WHERE t.id = $2
  AND t.deleted_at IS NULL
  AND (
        t.concurrency_cap IS NULL
     OR (SELECT count(*) FROM calls c
         WHERE c.tenant_id = t.id
           AND c.call_uuid <> $1) < t.concurrency_cap
      )
ON CONFLICT (call_uuid) DO UPDATE SET heartbeat_at = now()
RETURNING call_uuid`

// Heartbeat marks a call as still held.
//
// A controller that crashes leaves its rows claimed with nobody on the calls,
// and the tenant's effective capacity drops with nothing anywhere saying why.
// SweepStaleCalls reclaims anything that stops beating.
func (p *Pool) Heartbeat(ctx context.Context, uuid string) error {
	_, err := p.Exec(ctx,
		`UPDATE calls SET heartbeat_at = now() WHERE call_uuid = $1`, uuid)
	if err != nil {
		return fmt.Errorf("store: heartbeat for %s: %w", uuid, err)
	}
	return nil
}

// SetCallState records a transition.
func (p *Pool) SetCallState(ctx context.Context, uuid, state string, answeredAt time.Time) error {
	_, err := p.Exec(ctx, `
		UPDATE calls
		   SET state        = $2,
		       answered_at  = COALESCE(answered_at, $3),
		       heartbeat_at = now()
		 WHERE call_uuid = $1`,
		uuid, state, nullTimeArg(answeredAt))
	if err != nil {
		return fmt.Errorf("store: setting state of %s to %s: %w", uuid, state, err)
	}
	return nil
}

// FinishCall writes the permanent record and releases the slot, together.
//
// ONE TRANSACTION, and that is the point of the controller holding a database
// credential. Writing the record and releasing the slot as two separate
// operations means a crash between them either leaks a slot forever or loses a
// CDR for a call that definitely happened. Neither is recoverable afterwards,
// because nothing else knows the call existed.
//
// Safe for a call that was never claimed. A call refused before the claim —
// an unknown number — still deserves a record, and the DELETE simply matches
// nothing.
func (p *Pool) FinishCall(ctx context.Context, r calls.Record) error {
	if err := r.Validate(); err != nil {
		return err
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: beginning finish: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, insertHistorySQL, r.Args()...); err != nil {
		return fmt.Errorf("store: writing call record %s: %w", r.UUID, err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM calls WHERE call_uuid = $1`, r.UUID); err != nil {
		return fmt.Errorf("store: releasing slot for %s: %w", r.UUID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: committing finish for %s: %w", r.UUID, err)
	}
	return nil
}

// ON CONFLICT DO UPDATE, not DO NOTHING.
//
// # Why a second write happens at all
//
// A TRANSFERRED call touches this row twice. The AI leg ends when the caller is
// moved to a human, and the call carries on — so the controller writes once at
// the transfer and again when the call really ends.
//
// DO NOTHING meant the first write won: the record said the call ended at the
// transfer, ran for as long as the AI had it, and said nothing about the human
// who actually took it. Silently, because a discarded write reports success.
//
// # Merging, and which side wins each field
//
// The BEGINNING comes from whichever write saw it first — a later write knows
// nothing about when the call arrived or answered, and LEAST() over a NULL
// would erase a real timestamp, which is why COALESCE guards every one.
//
// The ENDING comes from the later write. Durations are recomputed from the
// merged timestamps rather than carried over, because the first write's
// talk_seconds measured only the AI leg.
//
// A retried write after a connection blip is still safe: it carries the same
// values, so the merge is a no-op.
const insertHistorySQL = `
INSERT INTO call_history (
    call_uuid, sip_call_id, tenant_id, direction,
    did, caller, callee,
    trunk_id, plan_id, handoff_target_id, livekit_room, node,
    invite_at, ring_at, answer_at, end_at,
    ring_seconds, talk_seconds, total_seconds,
    sip_code, hangup_cause, hangup_by, outcome, recording_url
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7,
    $8, $9, $10, $11, $12,
    $13, $14, $15, $16,
    $17, $18, $19,
    $20, $21, $22, $23, $24
)
ON CONFLICT (call_uuid) DO UPDATE SET
    -- The beginning: earliest wins, and never overwritten with NULL.
    invite_at = LEAST(COALESCE(call_history.invite_at, EXCLUDED.invite_at),
                      COALESCE(EXCLUDED.invite_at, call_history.invite_at)),
    ring_at   = LEAST(COALESCE(call_history.ring_at, EXCLUDED.ring_at),
                      COALESCE(EXCLUDED.ring_at, call_history.ring_at)),
    answer_at = LEAST(COALESCE(call_history.answer_at, EXCLUDED.answer_at),
                      COALESCE(EXCLUDED.answer_at, call_history.answer_at)),

    -- The ending: latest wins.
    end_at = GREATEST(COALESCE(call_history.end_at, EXCLUDED.end_at),
                      COALESCE(EXCLUDED.end_at, call_history.end_at)),

    -- Terminal facts describe how the call FINISHED, so the later write owns
    -- them — but only when it has something to say. A second write with no
    -- cause must not blank a cause the first one recorded.
    sip_code          = COALESCE(EXCLUDED.sip_code, call_history.sip_code),
    hangup_cause      = COALESCE(EXCLUDED.hangup_cause, call_history.hangup_cause),
    hangup_by         = COALESCE(EXCLUDED.hangup_by, call_history.hangup_by),
    -- 'transferred' is never downgraded.
    --
    -- Two goroutines write this row for a transferred call — the leg that was
    -- handed off, and the original still waiting for the channel to die — and
    -- either may land last. Both are written to say 'transferred', so today
    -- this clause changes nothing; it is here so that a third writer, or one
    -- of those two being changed later, cannot quietly turn a call that
    -- reached a human back into an ordinary bridged call.
    --
    -- The failure it guards against is invisible in the data: the row still
    -- looks complete and plausible, just no longer counted as a handoff.
    outcome           = CASE
                            WHEN call_history.outcome = 'transferred' THEN 'transferred'
                            ELSE COALESCE(EXCLUDED.outcome, call_history.outcome)
                        END,
    handoff_target_id = COALESCE(EXCLUDED.handoff_target_id, call_history.handoff_target_id),
    livekit_room      = COALESCE(EXCLUDED.livekit_room, call_history.livekit_room),
    recording_url     = COALESCE(EXCLUDED.recording_url, call_history.recording_url),
    callee            = COALESCE(EXCLUDED.callee, call_history.callee),

    -- Recomputed from the merged timestamps. The first write's talk_seconds
    -- measured the AI leg alone; carrying it over would under-report every
    -- transferred call.
    ring_seconds  = GREATEST(0, EXTRACT(EPOCH FROM (
                      COALESCE(LEAST(COALESCE(call_history.answer_at, EXCLUDED.answer_at),
                                     COALESCE(EXCLUDED.answer_at, call_history.answer_at)),
                               GREATEST(COALESCE(call_history.end_at, EXCLUDED.end_at),
                                        COALESCE(EXCLUDED.end_at, call_history.end_at)))
                      - LEAST(COALESCE(call_history.ring_at, EXCLUDED.ring_at),
                              COALESCE(EXCLUDED.ring_at, call_history.ring_at))))::int),
    talk_seconds  = GREATEST(0, EXTRACT(EPOCH FROM (
                      GREATEST(COALESCE(call_history.end_at, EXCLUDED.end_at),
                               COALESCE(EXCLUDED.end_at, call_history.end_at))
                      - LEAST(COALESCE(call_history.answer_at, EXCLUDED.answer_at),
                              COALESCE(EXCLUDED.answer_at, call_history.answer_at))))::int),
    total_seconds = GREATEST(0, EXTRACT(EPOCH FROM (
                      GREATEST(COALESCE(call_history.end_at, EXCLUDED.end_at),
                               COALESCE(EXCLUDED.end_at, call_history.end_at))
                      - LEAST(COALESCE(call_history.invite_at, EXCLUDED.invite_at),
                              COALESCE(EXCLUDED.invite_at, call_history.invite_at))))::int)`

// SweepStaleCalls releases slots whose controller stopped heartbeating.
//
// Without this, one crash permanently reduces a tenant's capacity, and the
// symptom is calls refused for congestion on a platform carrying no calls.
// Returns how many were reclaimed — worth logging, because a non-zero result
// means something died.
func (p *Pool) SweepStaleCalls(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := p.Exec(ctx,
		`DELETE FROM calls WHERE heartbeat_at < now() - $1::interval`,
		olderThan.String())
	if err != nil {
		return 0, fmt.Errorf("store: sweeping stale calls: %w", err)
	}
	return tag.RowsAffected(), nil
}

// CountActiveCalls reports live calls for a tenant, for the operator API.
func (p *Pool) CountActiveCalls(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := p.QueryRow(ctx,
		`SELECT count(*) FROM calls WHERE tenant_id = $1`, tenantID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: counting calls for %s: %w", tenantID, err)
	}
	return n, nil
}

func nullStringArg(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64Arg(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullTimeArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// liveCallSQL reads the row a call holds while it is up.
//
// The transferred leg needs it because of what FreeSWITCH does with a REFER:
// it looks the Refer-To user up in the dialplan, so the call comes back as a
// NEW inbound call dialling the literal word `handoff` — a destination no
// tenant owns and no DID table will ever match.
//
// What saves it is the UUID. FreeSWITCH transfers the EXISTING channel, so the
// second leg arrives carrying the same call_uuid as the first, and this row is
// still there holding the slot. The tenant is read from it rather than
// resolved from the number, which is the only way round the fact that the
// number carries no tenant at all.
//
// Same columns as LiveCalls above, deliberately: one shape for a call in
// progress, whether it is being listed for an operator or read by the leg that
// continues it.
const liveCallSQL = `
SELECT call_uuid, tenant_id, COALESCE(plan_id, ''), direction,
       COALESCE(did, ''), COALESCE(caller, ''), state,
       COALESCE(node, ''), COALESCE(livekit_room, ''),
       started_at, answered_at, heartbeat_at
  FROM calls
 WHERE call_uuid = $1`

// LiveCall returns the in-progress call for a UUID.
//
// Returns calls.ErrNoLiveCall when there is no such row, which is an ordinary
// outcome rather than a fault: a transfer arriving after its own call has been
// finalised looks exactly like this, and so does a `handoff` dialled by
// something that was never transferred at all.
func (p *Pool) LiveCall(ctx context.Context, uuid string) (calls.Live, error) {
	var l calls.Live

	err := p.QueryRow(ctx, liveCallSQL, uuid).Scan(
		&l.CallUUID, &l.TenantID, &l.PlanID, &l.Direction,
		&l.DID, &l.Caller, &l.State,
		&l.Node, &l.LiveKitRoom,
		&l.StartedAt, &l.AnsweredAt, &l.HeartbeatAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return calls.Live{}, calls.ErrNoLiveCall
	}
	if err != nil {
		return calls.Live{}, fmt.Errorf("store: reading live call %s: %w", uuid, err)
	}

	return l, nil
}
