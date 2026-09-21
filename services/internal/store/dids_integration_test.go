//go:build integration

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
)

// seedRoute puts one fully-configured tenant/plan/DID in place.
func seedRoute(t *testing.T, pool *Pool) {
	t.Helper()

	mustExec(t, pool, `INSERT INTO tenants (id, name) VALUES ('t1', 'One')`)
	mustExec(t, pool,
		`INSERT INTO inbound_plans (id, tenant_id, name, destination, recording)
		 VALUES ('p1', 't1', 'Main', 'ai', true)`)
	mustExec(t, pool,
		`INSERT INTO trunks (id, slug, name, type, carrier, sip_auth_method, sip_host)
		 VALUES (7, 'twilio-frankfurt', 'Twilio', 'bi_directional', 'twilio',
		         'ip_whitelisting', 'intron-beta.pstn.frankfurt.twilio.com')`)
	mustExec(t, pool,
		`INSERT INTO dids (number, tenant_id, plan_id, trunk_id)
		 VALUES ('+4930123456', 't1', 'p1', 7)`)
}

func TestResolveNumber_ExactDID(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedRoute(t, pool)

	r, err := pool.ResolveNumber(ctx, "+4930123456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if r.TenantID != "t1" || r.PlanID != "p1" {
		t.Errorf("wrong tenant/plan: %+v", r)
	}
	if !r.TenantActive || !r.DIDActive || !r.PlanActive {
		t.Errorf("want everything active: %+v", r)
	}
	if r.Destination != admission.DestinationAI || !r.Recording {
		t.Errorf("plan not carried through: %+v", r)
	}
	// The trunk comes from the DID row. That coupling is what makes a
	// caller-ID / trunk mismatch unrepresentable on egress.
	if r.TrunkID != 7 {
		t.Errorf("TrunkID = %d, want 7", r.TrunkID)
	}
}

func TestResolveNumber_UnknownIsNotFound(t *testing.T) {
	pool := migratedPool(t)
	seedRoute(t, pool)

	_, err := pool.ResolveNumber(context.Background(), "+4930999999")
	if !errors.Is(err, admission.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got: %v", err)
	}
}

// A number provisioned but never configured must resolve to "ours, no plan",
// not to "not ours". The two need different fixes and only one is a mistake —
// which is why the plan join is LEFT.
func TestResolveNumber_DIDWithNoPlan(t *testing.T) {
	pool := migratedPool(t)

	mustExec(t, pool, `INSERT INTO tenants (id, name) VALUES ('t1', 'One')`)
	mustExec(t, pool, `INSERT INTO dids (number, tenant_id) VALUES ('+4930123456', 't1')`)

	r, err := pool.ResolveNumber(context.Background(), "+4930123456")
	if err != nil {
		t.Fatalf("a DID with no plan must still resolve, got: %v", err)
	}
	if r.TenantID != "t1" {
		t.Errorf("TenantID = %q, want t1", r.TenantID)
	}
	if r.PlanID != "" {
		t.Errorf("PlanID = %q, want empty", r.PlanID)
	}
}

// A suspended tenant's numbers must still RESOLVE — the policy decision
// belongs to admission.evaluate, and a query that hid the row would report the
// number as unknown, which is the wrong thing to tell a customer asking why
// their calls stopped.
func TestResolveNumber_SuspendedTenantStillResolves(t *testing.T) {
	pool := migratedPool(t)
	seedRoute(t, pool)
	mustExec(t, pool, `UPDATE tenants SET status = 'suspended' WHERE id = 't1'`)

	r, err := pool.ResolveNumber(context.Background(), "+4930123456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TenantActive {
		t.Error("want TenantActive false")
	}
}

func TestResolveNumber_SoftDeletedIsGone(t *testing.T) {
	pool := migratedPool(t)
	seedRoute(t, pool)
	mustExec(t, pool, `UPDATE dids SET deleted_at = now() WHERE number = '+4930123456'`)

	_, err := pool.ResolveNumber(context.Background(), "+4930123456")
	if !errors.Is(err, admission.ErrNotFound) {
		t.Fatalf("want ErrNotFound for a deleted DID, got: %v", err)
	}
}

func TestResolveNumber_FallsBackToRange(t *testing.T) {
	pool := migratedPool(t)

	mustExec(t, pool, `INSERT INTO tenants (id, name) VALUES ('t1', 'One')`)
	mustExec(t, pool,
		`INSERT INTO inbound_plans (id, tenant_id, name, destination)
		 VALUES ('p1', 't1', 'Main', 'handoff')`)
	mustExec(t, pool,
		`INSERT INTO did_ranges (tenant_id, plan_id, number_range)
		 VALUES ('t1', 'p1', int8range(4930100000, 4930200000))`)

	r, err := pool.ResolveNumber(context.Background(), "+4930150000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TenantID != "t1" || r.Destination != admission.DestinationHandoff {
		t.Errorf("range did not resolve: %+v", r)
	}
	// The caller only ever sees the '+' form, whichever table answered.
	if r.Number != "+4930150000" {
		t.Errorf("Number = %q, want +4930150000", r.Number)
	}
}

// A number carved out of a block and pointed somewhere specific is the whole
// reason both tables exist. The specific record has to beat the general one,
// or the carve-out silently does nothing.
func TestResolveNumber_ExactDIDBeatsRange(t *testing.T) {
	pool := migratedPool(t)

	mustExec(t, pool, `INSERT INTO tenants (id, name) VALUES ('t1', 'One'), ('t2', 'Two')`)
	mustExec(t, pool,
		`INSERT INTO inbound_plans (id, tenant_id, name, destination)
		 VALUES ('p-range', 't1', 'Range', 'handoff'), ('p-exact', 't2', 'Exact', 'ai')`)
	mustExec(t, pool,
		`INSERT INTO did_ranges (tenant_id, plan_id, number_range)
		 VALUES ('t1', 'p-range', int8range(4930100000, 4930200000))`)
	mustExec(t, pool,
		`INSERT INTO dids (number, tenant_id, plan_id)
		 VALUES ('+4930150000', 't2', 'p-exact')`)

	r, err := pool.ResolveNumber(context.Background(), "+4930150000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TenantID != "t2" || r.PlanID != "p-exact" {
		t.Errorf("the exact DID should win, got tenant=%q plan=%q", r.TenantID, r.PlanID)
	}
}

// A number longer than int8 can hold cannot be in any range, and must be a
// miss rather than an error — otherwise malformed input reads as an outage and
// the caller hangs up 503 instead of 404.
func TestResolveNumber_OverlongNumberIsAMiss(t *testing.T) {
	pool := migratedPool(t)

	_, err := pool.ResolveNumber(context.Background(), "+99999999999999999999999")
	if !errors.Is(err, admission.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got: %v", err)
	}
}

func TestActiveRoutes_LoadsForWarming(t *testing.T) {
	pool := migratedPool(t)
	seedRoute(t, pool)
	mustExec(t, pool,
		`INSERT INTO dids (number, tenant_id, plan_id) VALUES ('+4930123457', 't1', 'p1')`)

	routes, err := pool.ActiveRoutes(context.Background(), 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("want 2 routes, got %d", len(routes))
	}
	if routes[0].Number != "+4930123456" {
		t.Errorf("want ordered by number, got %q first", routes[0].Number)
	}
}

// End to end through the cache: the service must resolve real SQL, then stop
// touching the database.
func TestAdmissionService_AgainstRealSQL(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedRoute(t, pool)

	svc := admission.New(pool, quietLogger(), admission.Options{})

	v, err := svc.Resolve(ctx, "004930123456") // deliberately not E.164
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Admitted {
		t.Fatalf("want admitted, got %q", v.Reason)
	}
	if v.TenantID != "t1" || v.Destination != admission.DestinationAI {
		t.Errorf("unexpected verdict: %+v", v)
	}

	// Suspend the tenant. The cached answer still admits, which is the
	// staleness budget working as designed, not a bug.
	mustExec(t, pool, `UPDATE tenants SET status = 'suspended' WHERE id = 't1'`)

	v, err = svc.Resolve(ctx, "+4930123456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Admitted {
		t.Error("want the cached answer within the TTL")
	}

	// After invalidation the new state must be visible.
	svc.Invalidate("+4930123456")

	v, err = svc.Resolve(ctx, "+4930123456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Admitted {
		t.Error("want refused once the cache is invalidated")
	}
	if v.Reason != admission.ReasonTenantSuspended {
		t.Errorf("Reason = %q, want %q", v.Reason, admission.ReasonTenantSuspended)
	}
}
