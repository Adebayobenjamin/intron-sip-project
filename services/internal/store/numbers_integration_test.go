//go:build integration

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/numrange"
)

// The fixture every test here builds on.
//
//	trunk 1   platform (no tenant)   +2348000000100 – +2348000000199
//	trunk 2   acme's own SBC (BYOT)  +4930150000    – +4930150009
//
//	line      acme, inbound, platform trunk, +…100 – +…149, ai, ai_only
type fixture struct {
	platform, byot int64
	acmeInbound    int64
}

func block(from, to string) numrange.Bounds {
	_, b, err := numrange.Parse(numrange.Range{From: from, To: to})
	if err != nil {
		panic(err)
	}
	return b
}

func seedNumbers(t *testing.T, pool *Pool) fixture {
	t.Helper()
	ctx := context.Background()

	mustExec(t, pool, `INSERT INTO tenants (id, name) VALUES ('acme', 'Acme'), ('beta', 'Beta')`)

	platform, err := pool.CreateTrunk(ctx, Trunk{
		Slug: "twilio", Name: "Twilio", Carrier: "twilio",
		SIPAuthMethod: "ip_whitelisting", SIPHost: "x.pstn.twilio.com",
	}, []numrange.Bounds{block("+2348000000100", "+2348000000199")})
	if err != nil {
		t.Fatalf("platform trunk: %v", err)
	}

	byot, err := pool.CreateTrunk(ctx, Trunk{
		TenantID: "acme", Slug: "acme-sbc", Name: "Acme SBC", Carrier: "acme",
		SIPAuthMethod: "ip_whitelisting", SIPHost: "sbc.acme", SIPPort: 5080,
	}, []numrange.Bounds{block("+4930150000", "+4930150009")})
	if err != nil {
		t.Fatalf("byot trunk: %v", err)
	}

	line, err := pool.CreateTrafficConfig(ctx, TrafficConfigCreate{
		TenantID: "acme", TrunkID: platform.ID, Name: "Main", Direction: "inbound",
		DIDRanges:   []numrange.Bounds{block("+2348000000100", "+2348000000149")},
		Destination: "ai", RecordingMode: "ai_only",
	})
	if err != nil {
		t.Fatalf("inbound line: %v", err)
	}

	return fixture{platform: platform.ID, byot: byot.ID, acmeInbound: line.ID}
}

func TestResolveNumber_LineOnThePlatformTrunk(t *testing.T) {
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	r, err := pool.ResolveNumber(context.Background(), "+2348000000123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The tenant comes from the LINE: the platform trunk has none.
	if r.TenantID != "acme" || r.TrafficConfigID != f.acmeInbound {
		t.Errorf("wrong tenant/line: %+v", r)
	}
	if !r.TenantActive || !r.ConfigActive {
		t.Errorf("want everything active: %+v", r)
	}
	if r.Destination != admission.DestinationAI || r.RecordingMode != admission.RecordingAIOnly {
		t.Errorf("line not carried through: %+v", r)
	}
	// The trunk the number is on — what an E.164 handoff will leave by.
	if r.TrunkID != f.platform {
		t.Errorf("TrunkID = %d, want the platform trunk %d", r.TrunkID, f.platform)
	}
	if r.Number != "+2348000000123" {
		t.Errorf("Number = %q", r.Number)
	}
}

func TestResolveNumber_UnknownIsNotFound(t *testing.T) {
	pool := migratedPool(t)
	seedNumbers(t, pool)

	_, err := pool.ResolveNumber(context.Background(), "+4930999999")
	if !errors.Is(err, admission.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got: %v", err)
	}
}

// A platform number with no line is ours but nobody's yet: it must resolve —
// "ours, nothing configured" — with no tenant to name.
func TestResolveNumber_PlatformNumberWithNoLine(t *testing.T) {
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	r, err := pool.ResolveNumber(context.Background(), "+2348000000175")
	if err != nil {
		t.Fatalf("a carried number with no line must still resolve, got: %v", err)
	}
	if r.TenantID != "" || r.TrafficConfigID != 0 || r.TrunkID != f.platform {
		t.Errorf("want no tenant, no line, the platform trunk: %+v", r)
	}

	svc := admission.New(pool, quietLogger(), admission.Options{})
	v, _ := svc.Resolve(context.Background(), "+2348000000175")
	if v.Reason != admission.ReasonNoTrafficConfig {
		t.Errorf("Reason = %q, want %q", v.Reason, admission.ReasonNoTrafficConfig)
	}
}

// On a client's own trunk an unconfigured number is still the client's.
func TestResolveNumber_BYOTNumberWithNoLineBelongsToTheTrunksTenant(t *testing.T) {
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	r, err := pool.ResolveNumber(context.Background(), "+4930150005")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TenantID != "acme" || r.TrafficConfigID != 0 || r.TrunkID != f.byot {
		t.Errorf("want acme, no line, the BYOT trunk: %+v", r)
	}
}

// An OUTBOUND line says nothing about what happens when the number rings.
func TestResolveNumber_IgnoresOutboundLines(t *testing.T) {
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	if _, err := pool.CreateTrafficConfig(context.Background(), TrafficConfigCreate{
		TenantID: "beta", TrunkID: f.platform, Name: "Campaign", Direction: "outbound",
		DIDRanges:   []numrange.Bounds{block("+2348000000150", "+2348000000199")},
		Destination: "ai",
	}); err != nil {
		t.Fatal(err)
	}

	r, err := pool.ResolveNumber(context.Background(), "+2348000000160")
	if err != nil {
		t.Fatal(err)
	}
	if r.TrafficConfigID != 0 || r.TenantID != "" {
		t.Errorf("an outbound line answered an inbound lookup: %+v", r)
	}
}

// A suspended tenant's numbers must still RESOLVE — the policy decision belongs
// to admission.evaluate, and a query that hid the row would report the number
// as unknown, which is the wrong thing to tell a customer asking why their
// calls stopped.
func TestResolveNumber_SuspendedTenantStillResolves(t *testing.T) {
	pool := migratedPool(t)
	seedNumbers(t, pool)
	mustExec(t, pool, `UPDATE tenants SET status = 'suspended' WHERE id = 'acme'`)

	r, err := pool.ResolveNumber(context.Background(), "+2348000000123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TenantActive {
		t.Error("want TenantActive false")
	}
}

func TestResolveNumber_DeletedTrunkIsGone(t *testing.T) {
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	if err := pool.DeleteTrunk(context.Background(), f.byot, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ResolveNumber(context.Background(), "+4930150005"); !errors.Is(err, admission.ErrNotFound) {
		t.Fatalf("want ErrNotFound for a number on a deleted trunk, got: %v", err)
	}
}

// Malformed numbers are misses, not errors — otherwise bad input reads as an
// outage and the caller hangs up 503 instead of 404.
func TestResolveNumber_MalformedIsAMiss(t *testing.T) {
	pool := migratedPool(t)

	for _, n := range []string{"+99999999999999999999999", "+0123456"} {
		if _, err := pool.ResolveNumber(context.Background(), n); !errors.Is(err, admission.ErrNotFound) {
			t.Errorf("%s: want ErrNotFound, got: %v", n, err)
		}
	}
}

func TestActiveRoutes_EnumeratesSmallInboundBlocks(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	routes, err := pool.ActiveRoutes(ctx, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// +…100 to +…149: fifty numbers, each resolved the same way the call path
	// would.
	if len(routes) != 50 {
		t.Fatalf("want 50 routes, got %d", len(routes))
	}
	for _, r := range routes {
		if r.TenantID != "acme" || r.TrafficConfigID != f.acmeInbound || r.TrunkID != f.platform {
			t.Fatalf("warmed route disagrees with the call path: %+v", r)
		}
	}

	limited, err := pool.ActiveRoutes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 10 {
		t.Errorf("limit ignored: got %d", len(limited))
	}
}

// A huge block is not enumerated: materialising it would build every number
// in it before the limit applies.
func TestActiveRoutes_SkipsHugeBlocks(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	mustExec(t, pool, `INSERT INTO tenants (id, name) VALUES ('big', 'Big')`)

	trunk, err := pool.CreateTrunk(ctx, Trunk{
		TenantID: "big", Slug: "big", Name: "Big", Carrier: "x", SIPAuthMethod: "ip_whitelisting",
	}, []numrange.Bounds{block("+2349000000000", "+2349000999999")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.CreateTrafficConfig(ctx, TrafficConfigCreate{
		TenantID: "big", TrunkID: trunk.ID, Name: "All", Direction: "inbound",
		DIDRanges: []numrange.Bounds{block("+2349000000000", "+2349000999999")}, Destination: "ai",
	}); err != nil {
		t.Fatal(err)
	}

	routes, err := pool.ActiveRoutes(ctx, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 0 {
		t.Errorf("a million-number block was enumerated: %d routes", len(routes))
	}
}

// End to end through the cache: the service must resolve real SQL, then stop
// touching the database.
func TestAdmissionService_AgainstRealSQL(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedNumbers(t, pool)

	svc := admission.New(pool, quietLogger(), admission.Options{})

	v, err := svc.Resolve(ctx, "002348000000123") // deliberately not E.164
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Admitted {
		t.Fatalf("want admitted, got %q", v.Reason)
	}
	if v.TenantID != "acme" || v.Destination != admission.DestinationAI {
		t.Errorf("unexpected verdict: %+v", v)
	}

	// Suspend the tenant. The cached answer still admits, which is the
	// staleness budget working as designed, not a bug.
	mustExec(t, pool, `UPDATE tenants SET status = 'suspended' WHERE id = 'acme'`)

	if v, _ = svc.Resolve(ctx, "+2348000000123"); !v.Admitted {
		t.Error("want the cached answer within the TTL")
	}

	// After invalidation the new state must be visible.
	svc.InvalidateAll()

	v, err = svc.Resolve(ctx, "+2348000000123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Admitted || v.Reason != admission.ReasonTenantSuspended {
		t.Errorf("want refused as suspended once invalidated, got admitted=%v reason=%q", v.Admitted, v.Reason)
	}
}

// ---------------------------------------------------------------------------
// GET /v1/numbers/{e164}
// ---------------------------------------------------------------------------

func TestNumber_PlatformCallerSeesBothDirections(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	if _, err := pool.CreateTrafficConfig(ctx, TrafficConfigCreate{
		TenantID: "beta", TrunkID: f.platform, Name: "Campaign", Direction: "outbound",
		DIDRanges: []numrange.Bounds{block("+2348000000100", "+2348000000149")}, Destination: "ai",
	}); err != nil {
		t.Fatal(err)
	}

	info, err := pool.Number(ctx, "+2348000000123", "")
	if err != nil {
		t.Fatal(err)
	}
	if info.Trunk.ID != f.platform || !info.Trunk.Platform {
		t.Errorf("trunk = %+v", info.Trunk)
	}
	if info.Inbound == nil || info.Inbound.TenantID != "acme" {
		t.Errorf("inbound = %+v", info.Inbound)
	}
	if info.Outbound == nil || info.Outbound.TenantID != "beta" {
		t.Errorf("outbound = %+v", info.Outbound)
	}
}

// A platform number leased inbound to one tenant and outbound to another: each
// sees only their own line.
func TestNumber_TenantSeesOnlyTheirOwnLines(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	if _, err := pool.CreateTrafficConfig(ctx, TrafficConfigCreate{
		TenantID: "beta", TrunkID: f.platform, Name: "Campaign", Direction: "outbound",
		DIDRanges: []numrange.Bounds{block("+2348000000100", "+2348000000149")}, Destination: "ai",
	}); err != nil {
		t.Fatal(err)
	}

	info, err := pool.Number(ctx, "+2348000000123", "beta")
	if err != nil {
		t.Fatal(err)
	}
	if info.Inbound != nil {
		t.Error("beta was shown acme's inbound line")
	}
	if info.Outbound == nil {
		t.Error("beta was not shown its own outbound line")
	}

	// Someone with no stake in the number gets the same answer as for a
	// number nobody owns.
	mustExec(t, pool, `INSERT INTO tenants (id, name) VALUES ('other', 'Other')`)
	if _, err := pool.Number(ctx, "+2348000000123", "other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unrelated tenant could see the number: %v", err)
	}
}

// On their own trunk a number is the tenant's even with no line on it.
func TestNumber_BYOTNumberVisibleToItsTenant(t *testing.T) {
	pool := migratedPool(t)
	seedNumbers(t, pool)

	info, err := pool.Number(context.Background(), "+4930150005", "acme")
	if err != nil {
		t.Fatalf("acme could not see a number on its own trunk: %v", err)
	}
	if info.Trunk.Platform || info.Trunk.TenantID != "acme" {
		t.Errorf("trunk = %+v", info.Trunk)
	}
}
