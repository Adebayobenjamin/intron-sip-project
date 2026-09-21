//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
)

func seedTenant(t *testing.T, pool *Pool, id string, cap any) {
	t.Helper()
	mustExec(t, pool,
		`INSERT INTO tenants (id, name, concurrency_cap) VALUES ($1, $1, $2)`, id, cap)
}

func claim(uuid, tenant string) calls.Claim {
	return calls.Claim{
		UUID:      uuid,
		TenantID:  tenant,
		Direction: calls.DirectionInbound,
		DID:       "+4930123456",
		Caller:    "+2348012345678",
		Node:      "beta-fs-1",
	}
}

func record(uuid, tenant string) calls.Record {
	now := time.Now()
	return calls.Record{
		UUID:        uuid,
		TenantID:    tenant,
		Direction:   calls.DirectionInbound,
		DID:         "+4930123456",
		Caller:      "+2348012345678",
		InviteAt:    now.Add(-30 * time.Second),
		EndAt:       now,
		HangupCause: "NORMAL_CLEARING",
		HangupBy:    calls.HangupByCaller,
		Outcome:     "ok",
	}
}

func TestClaimCall_TakesASlot(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedTenant(t, pool, "t1", nil)

	if err := pool.ClaimCall(ctx, claim("call-1", "t1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	n, err := pool.CountActiveCalls(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("want 1 active call, got %d", n)
	}
}

// A NULL cap means unlimited, which is where a tenant starts.
func TestClaimCall_NullCapIsUnlimited(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedTenant(t, pool, "t1", nil)

	for i := range 25 {
		if err := pool.ClaimCall(ctx, claim(fmt.Sprintf("call-%d", i), "t1")); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
	}
}

func TestClaimCall_RefusesPastTheCap(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedTenant(t, pool, "t1", 2)

	for i := range 2 {
		if err := pool.ClaimCall(ctx, claim(fmt.Sprintf("call-%d", i), "t1")); err != nil {
			t.Fatalf("claim %d should fit under the cap: %v", i, err)
		}
	}

	err := pool.ClaimCall(ctx, claim("call-3", "t1"))
	if !errors.Is(err, calls.ErrAtCapacity) {
		t.Fatalf("want ErrAtCapacity, got %v", err)
	}
}

// THE TEST THIS WHOLE DESIGN TURNS ON.
//
// The obvious implementation is a conditional insert that counts rows in the
// same statement. It looks atomic and is not: under READ COMMITTED the
// subquery cannot see rows from transactions that have not committed, so N
// simultaneous calls all count the same total and all pass a cap of one.
//
// # This test needs a big pool, and that is not incidental
//
// The first version used the shared four-connection pool and PASSED against
// the broken implementation — four connections is not enough concurrency for
// the window to open, so the test proved nothing. With thirty it fails
// reliably without the advisory lock (4 claims against a cap of 3) and passes
// with it.
//
// Worth remembering if this is ever refactored: a concurrency test that cannot
// fail is worse than no test, because it is read as evidence.
func TestClaimCall_CapHoldsUnderConcurrency(t *testing.T) {
	ctx := context.Background()

	cfg := testConfig(newTestDB(t))
	cfg.MaxConns = 30

	if err := Migrate(ctx, cfg, quietLogger()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	pool, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)

	const (
		cap      = 3
		attempts = 40
	)
	seedTenant(t, pool, "t1", cap)

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		start     = make(chan struct{})
	)

	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			// Released together, so the claims genuinely overlap rather than
			// queueing behind each other's startup.
			<-start

			err := pool.ClaimCall(ctx, claim(fmt.Sprintf("call-%d", i), "t1"))
			switch {
			case err == nil:
				mu.Lock()
				succeeded++
				mu.Unlock()
			case errors.Is(err, calls.ErrAtCapacity):
				// Expected for most of them.
			default:
				t.Errorf("claim %d: unexpected error: %v", i, err)
			}
		}(i)
	}

	close(start)
	wg.Wait()

	if succeeded != cap {
		t.Errorf("%d claims succeeded against a cap of %d — the cap is not atomic",
			succeeded, cap)
	}

	n, cerr := pool.CountActiveCalls(ctx, "t1")
	if cerr != nil {
		t.Fatal(cerr)
	}
	if n != cap {
		t.Errorf("%d rows in calls against a cap of %d", n, cap)
	}
}

// A retried claim for a call already held must succeed, not report the tenant
// as full. The count excludes the call's own row precisely for this.
func TestClaimCall_RetryIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedTenant(t, pool, "t1", 1)

	if err := pool.ClaimCall(ctx, claim("call-1", "t1")); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	// Same call again — a network blip and a retry, at a cap of one.
	if err := pool.ClaimCall(ctx, claim("call-1", "t1")); err != nil {
		t.Fatalf("re-claiming the same call must succeed, got: %v", err)
	}

	n, _ := pool.CountActiveCalls(ctx, "t1")
	if n != 1 {
		t.Errorf("want 1 row after a retry, got %d", n)
	}
}

// Caps are per tenant. One tenant filling up must not affect another.
func TestClaimCall_CapsAreIndependent(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedTenant(t, pool, "t1", 1)
	seedTenant(t, pool, "t2", 1)

	if err := pool.ClaimCall(ctx, claim("call-1", "t1")); err != nil {
		t.Fatal(err)
	}
	if err := pool.ClaimCall(ctx, claim("call-2", "t2")); err != nil {
		t.Fatalf("a different tenant must be unaffected, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Finishing a call
// ---------------------------------------------------------------------------

// The record is written and the slot released in one transaction. Doing them
// separately means a crash in between either leaks a slot forever or loses the
// record of a call that definitely happened.
func TestFinishCall_WritesRecordAndReleasesSlot(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedTenant(t, pool, "t1", nil)

	if err := pool.ClaimCall(ctx, claim("call-1", "t1")); err != nil {
		t.Fatal(err)
	}
	if err := pool.FinishCall(ctx, record("call-1", "t1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	n, _ := pool.CountActiveCalls(ctx, "t1")
	if n != 0 {
		t.Errorf("want the slot released, got %d active", n)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM call_history WHERE call_uuid = 'call-1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("want 1 call_history row, got %d", count)
	}
}

// A call refused before any claim — an unknown number — still deserves a
// record, and the DELETE simply matches nothing.
func TestFinishCall_WorksWithoutAClaim(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedTenant(t, pool, "t1", nil)

	if err := pool.FinishCall(ctx, record("never-claimed", "t1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A retried write must not produce a second record for one call.
func TestFinishCall_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedTenant(t, pool, "t1", nil)

	for range 3 {
		if err := pool.FinishCall(ctx, record("call-1", "t1")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM call_history WHERE call_uuid = 'call-1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("want 1 record after 3 writes, got %d", count)
	}
}

// A record with no tenant would be written successfully and then be invisible
// to everyone, because every query is scoped by tenant.
func TestFinishCall_RefusesARecordWithNoTenant(t *testing.T) {
	pool := migratedPool(t)

	r := record("call-1", "")
	if err := pool.FinishCall(context.Background(), r); err == nil {
		t.Fatal("want an error for a record with no tenant")
	}
}

// Never answered means no talk time — not a duration measured from the zero
// instant.
func TestFinishCall_UnansweredCallHasNoTalkTime(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedTenant(t, pool, "t1", nil)

	if err := pool.FinishCall(ctx, record("call-1", "t1")); err != nil {
		t.Fatal(err)
	}

	var (
		answerAt *time.Time
		talk     int
		total    int
	)
	err := pool.QueryRow(ctx,
		`SELECT answer_at, talk_seconds, total_seconds
		   FROM call_history WHERE call_uuid = 'call-1'`).Scan(&answerAt, &talk, &total)
	if err != nil {
		t.Fatal(err)
	}

	if answerAt != nil {
		t.Errorf("answer_at = %v, want NULL", answerAt)
	}
	if talk != 0 {
		t.Errorf("talk_seconds = %d, want 0", talk)
	}
	if total != 30 {
		t.Errorf("total_seconds = %d, want 30", total)
	}
}

// ---------------------------------------------------------------------------
// The sweeper
// ---------------------------------------------------------------------------

// A controller that is killed leaves its rows claimed with nobody on the
// calls. Without the sweep the tenant's capacity drops permanently, and the
// symptom is congestion on a platform carrying nothing.
func TestSweepStaleCalls_ReclaimsAbandonedSlots(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedTenant(t, pool, "t1", nil)

	if err := pool.ClaimCall(ctx, claim("live", "t1")); err != nil {
		t.Fatal(err)
	}
	if err := pool.ClaimCall(ctx, claim("abandoned", "t1")); err != nil {
		t.Fatal(err)
	}

	mustExec(t, pool,
		`UPDATE calls SET heartbeat_at = now() - interval '1 hour' WHERE call_uuid = 'abandoned'`)

	n, err := pool.SweepStaleCalls(ctx, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("want 1 slot reclaimed, got %d", n)
	}

	active, _ := pool.CountActiveCalls(ctx, "t1")
	if active != 1 {
		t.Errorf("want the live call untouched, got %d active", active)
	}
}

// A call that is beating must survive the sweep, or a healthy call loses its
// slot and the cap starts refusing calls that should connect.
func TestHeartbeat_KeepsASlot(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedTenant(t, pool, "t1", nil)

	if err := pool.ClaimCall(ctx, claim("call-1", "t1")); err != nil {
		t.Fatal(err)
	}

	mustExec(t, pool,
		`UPDATE calls SET heartbeat_at = now() - interval '1 hour' WHERE call_uuid = 'call-1'`)

	if err := pool.Heartbeat(ctx, "call-1"); err != nil {
		t.Fatal(err)
	}

	n, err := pool.SweepStaleCalls(ctx, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a beating call was swept: %d rows", n)
	}
}

// ---------------------------------------------------------------------------
// The transferred-call merge
// ---------------------------------------------------------------------------

// A TRANSFERRED call writes this row twice: once when the AI leg ends, once
// when the call really ends. DO NOTHING meant the first write won — the record
// said the call ended at the transfer and never mentioned the human who took
// it, silently, because a discarded write reports success.
func TestFinishCall_TransferredCallMergesBothWrites(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedTenant(t, pool, "t1", nil)

	invite := time.Now().Add(-5 * time.Minute)
	answer := invite.Add(2 * time.Second)

	// First write: the AI leg ended after 60 seconds.
	aiLeg := calls.Record{
		UUID:        "call-1",
		TenantID:    "t1",
		Direction:   calls.DirectionInbound,
		DID:         "+4930123456",
		InviteAt:    invite,
		RingAt:      invite,
		AnswerAt:    answer,
		EndAt:       answer.Add(60 * time.Second),
		LiveKitRoom: "intron_sip__+234",
		Outcome:     "transferred",
	}
	if err := pool.FinishCall(ctx, aiLeg); err != nil {
		t.Fatal(err)
	}

	// Second write: the human took it and the call ran another four minutes.
	human := aiLeg
	human.EndAt = answer.Add(300 * time.Second)
	human.HandoffTargetID = 0 // no target row seeded; the column stays null
	human.HangupCause = "NORMAL_CLEARING"
	human.HangupBy = calls.HangupByCaller
	human.Outcome = "transferred"
	human.LiveKitRoom = "" // the final write knows nothing about the AI leg

	if err := pool.FinishCall(ctx, human); err != nil {
		t.Fatal(err)
	}

	var (
		count       int
		inviteAt    time.Time
		answerAt    time.Time
		endAt       time.Time
		talk, total int
		cause, by   string
		room        string
	)
	if err := pool.QueryRow(ctx, `
		SELECT count(*) OVER (), invite_at, answer_at, end_at,
		       talk_seconds, total_seconds,
		       COALESCE(hangup_cause,''), COALESCE(hangup_by,''), COALESCE(livekit_room,'')
		  FROM call_history WHERE call_uuid = 'call-1'`).
		Scan(&count, &inviteAt, &answerAt, &endAt, &talk, &total, &cause, &by, &room); err != nil {
		t.Fatal(err)
	}

	if count != 1 {
		t.Fatalf("want one row for one call, got %d", count)
	}

	// The beginning comes from the first write...
	if inviteAt.Unix() != invite.Unix() {
		t.Errorf("invite_at moved: %v, want %v", inviteAt, invite)
	}
	// ...and the ending from the second.
	if endAt.Unix() != human.EndAt.Unix() {
		t.Errorf("end_at = %v, want the later ending %v", endAt, human.EndAt)
	}

	// Durations recomputed across BOTH legs. The AI leg alone was 60s; the
	// whole call was 300s, and reporting 60 would under-report every
	// transferred call.
	if talk != 300 {
		t.Errorf("talk_seconds = %d, want 300 across both legs", talk)
	}
	if total != 302 {
		t.Errorf("total_seconds = %d, want 302", total)
	}

	// The later write owns how the call finished...
	if cause != "NORMAL_CLEARING" || by != calls.HangupByCaller {
		t.Errorf("terminal fields = %q/%q", cause, by)
	}
	// ...but must not blank what only the first write knew.
	if room != "intron_sip__+234" {
		t.Errorf("livekit_room = %q; the second write erased it", room)
	}
}

// A retried write after a connection blip carries the same values, so the merge
// must be a no-op rather than corrupting the row.
func TestFinishCall_RetryIsStillIdempotentAfterTheUpsert(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedTenant(t, pool, "t1", nil)

	r := record("call-1", "t1")
	for range 3 {
		if err := pool.FinishCall(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	var count, total int
	if err := pool.QueryRow(ctx,
		`SELECT count(*), max(total_seconds) FROM call_history WHERE call_uuid = 'call-1'`).
		Scan(&count, &total); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("want 1 row, got %d", count)
	}
	if total != 30 {
		t.Errorf("total_seconds = %d, want 30 — a retry changed the duration", total)
	}
}

// Two goroutines write this row for a transferred call and either may land
// last. Both are written to say 'transferred', so this clause changes nothing
// today — it is here so a later change to either side, or a third writer,
// cannot quietly turn a call that reached a human back into an ordinary one.
//
// The failure it guards against leaves the row looking complete and plausible,
// just no longer counted as a handoff.
func TestFinishCall_TransferredOutcomeIsNeverDowngraded(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedTenant(t, pool, "t1", nil)

	r := record("call-1", "t1")
	r.Outcome = "transferred"
	if err := pool.FinishCall(ctx, r); err != nil {
		t.Fatal(err)
	}

	// A later write reporting something more ordinary must not take it back.
	later := record("call-1", "t1")
	later.Outcome = "bridged"
	later.EndAt = r.EndAt.Add(2 * time.Minute)
	if err := pool.FinishCall(ctx, later); err != nil {
		t.Fatal(err)
	}

	var outcome string
	var total int
	if err := pool.QueryRow(ctx,
		`SELECT outcome, total_seconds FROM call_history WHERE call_uuid = 'call-1'`).
		Scan(&outcome, &total); err != nil {
		t.Fatal(err)
	}

	if outcome != "transferred" {
		t.Errorf("outcome = %q; a later write downgraded a transferred call", outcome)
	}
	// The rest of the later write must still merge — the guard is on one field,
	// not a blanket refusal to update.
	if total != 150 {
		t.Errorf("total_seconds = %d, want 150; the guard blocked the whole merge", total)
	}
}

// The guard must not pin an outcome that was never 'transferred'.
func TestFinishCall_OrdinaryOutcomeStillUpdates(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedTenant(t, pool, "t1", nil)

	r := record("call-1", "t1")
	r.Outcome = "bridged"
	if err := pool.FinishCall(ctx, r); err != nil {
		t.Fatal(err)
	}

	later := record("call-1", "t1")
	later.Outcome = "transferred"
	if err := pool.FinishCall(ctx, later); err != nil {
		t.Fatal(err)
	}

	var outcome string
	if err := pool.QueryRow(ctx,
		`SELECT outcome FROM call_history WHERE call_uuid = 'call-1'`).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome != "transferred" {
		t.Errorf("outcome = %q, want transferred — a call that later reached a "+
			"human must be recordable as one", outcome)
	}
}
