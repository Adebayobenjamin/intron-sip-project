//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/numrange"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/route"
)

// wantConflict fails unless err is a ConflictError whose message mentions
// want — the message is what the API returns, so it is part of the contract.
func wantConflict(t *testing.T, err error, want string) {
	t.Helper()
	var c *ConflictError
	if !errors.As(err, &c) {
		t.Fatalf("want a ConflictError, got: %v", err)
	}
	if !strings.Contains(c.Message, want) {
		t.Errorf("message %q should mention %q", c.Message, want)
	}
}

// ---------------------------------------------------------------------------
// The rules, as the API sees them
// ---------------------------------------------------------------------------

func TestTrafficConfig_RefusesAnOverlapInTheSameDirection(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	_, err := pool.CreateTrafficConfig(ctx, TrafficConfigCreate{
		TenantID: "beta", TrunkID: f.platform, Name: "Steal", Direction: "inbound",
		DIDRanges: []numrange.Bounds{block("+2348000000140", "+2348000000160")}, Destination: "ai",
	})
	wantConflict(t, err, "already claims")

	// The same numbers outbound are a different line, and allowed.
	if _, err := pool.CreateTrafficConfig(ctx, TrafficConfigCreate{
		TenantID: "beta", TrunkID: f.platform, Name: "Campaign", Direction: "outbound",
		DIDRanges: []numrange.Bounds{block("+2348000000140", "+2348000000160")}, Destination: "ai",
	}); err != nil {
		t.Errorf("an outbound line over inbound numbers was refused: %v", err)
	}
}

func TestTrafficConfig_RefusesNumbersOutsideTheTrunk(t *testing.T) {
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	_, err := pool.CreateTrafficConfig(context.Background(), TrafficConfigCreate{
		TenantID: "acme", TrunkID: f.platform, Name: "Out", Direction: "inbound",
		DIDRanges: []numrange.Bounds{block("+2348000000190", "+2348000000210")}, Destination: "ai",
	})
	wantConflict(t, err, "must lie within")
}

// One tenant routing calls over another tenant's carrier account.
func TestTrafficConfig_RefusesAnotherTenantsTrunk(t *testing.T) {
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	_, err := pool.CreateTrafficConfig(context.Background(), TrafficConfigCreate{
		TenantID: "beta", TrunkID: f.byot, Name: "X", Direction: "inbound",
		DIDRanges: []numrange.Bounds{block("+4930150000", "+4930150004")}, Destination: "ai",
	})
	wantConflict(t, err, "another tenant")
}

func TestTrunk_RefusesNumbersAnotherTrunkCarries(t *testing.T) {
	pool := migratedPool(t)
	seedNumbers(t, pool)

	_, err := pool.CreateTrunk(context.Background(), Trunk{
		Slug: "dup", Name: "Dup", Carrier: "x", SIPAuthMethod: "ip_whitelisting",
	}, []numrange.Bounds{block("+2348000000150", "+2348000000250")})
	wantConflict(t, err, "another trunk")
}

func TestTrunk_RefusesShrinkingOrDeletingUnderALine(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	smaller := []numrange.Bounds{block("+2348000000120", "+2348000000199")}
	_, err := pool.UpdateTrunk(ctx, f.platform, "", TrunkUpdate{DIDRanges: &smaller})
	wantConflict(t, err, "removes from trunk")

	wantConflict(t, pool.DeleteTrunk(ctx, f.platform, ""), "still has traffic configs")

	// Growing is fine.
	larger := []numrange.Bounds{block("+2348000000100", "+2348000000299")}
	if _, err := pool.UpdateTrunk(ctx, f.platform, "", TrunkUpdate{DIDRanges: &larger}); err != nil {
		t.Errorf("growing a trunk was refused: %v", err)
	}
}

func TestTrunk_SlugTakenIsAConflict(t *testing.T) {
	pool := migratedPool(t)
	seedNumbers(t, pool)

	_, err := pool.CreateTrunk(context.Background(), Trunk{
		Slug: "twilio", Name: "Again", Carrier: "twilio", SIPAuthMethod: "ip_whitelisting",
	}, nil)
	wantConflict(t, err, "slug already exists")
}

func TestHandoffTarget_RefusesAnOverrideOnAnotherTenantsLine(t *testing.T) {
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	_, err := pool.CreateHandoffTarget(context.Background(), HandoffTarget{
		TenantID: "beta", TrafficConfigID: &f.acmeInbound,
		Name: "x", Kind: route.KindSIPURI, Destination: "sip:q@beta", Enabled: true,
	})
	wantConflict(t, err, "does not exist for this tenant")
}

// ---------------------------------------------------------------------------
// Ranges round trip
// ---------------------------------------------------------------------------

// Adjacent blocks are merged into the canonical form Postgres compares
// against, and come back that way.
func TestTrunk_AdjacentBlocksComeBackMerged(t *testing.T) {
	pool := migratedPool(t)

	tr, err := pool.CreateTrunk(context.Background(), Trunk{
		Slug: "m", Name: "M", Carrier: "x", SIPAuthMethod: "ip_whitelisting",
	}, []numrange.Bounds{
		block("+2348000000200", "+2348000000299"),
		block("+2348000000100", "+2348000000199"),
		block("+2348000000500", "+2348000000500"),
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []numrange.Range{
		{From: "+2348000000100", To: "+2348000000299"},
		{From: "+2348000000500", To: "+2348000000500"},
	}
	if len(tr.DIDRanges) != len(want) {
		t.Fatalf("DIDRanges = %+v, want %+v", tr.DIDRanges, want)
	}
	for i := range want {
		if tr.DIDRanges[i] != want[i] {
			t.Errorf("DIDRanges[%d] = %+v, want %+v", i, tr.DIDRanges[i], want[i])
		}
	}
}

func TestTrafficConfig_UpdateIsPartial(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	full := "full"
	c, err := pool.UpdateTrafficConfig(ctx, f.acmeInbound, "acme", TrafficConfigUpdate{
		RecordingMode: &full,
		Metadata:      json.RawMessage(`{"queue":"lagos"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.RecordingMode != "full" || c.Destination != "ai" || c.Name != "Main" {
		t.Errorf("partial update touched other fields: %+v", c)
	}
	if len(c.DIDRanges) != 1 || c.DIDRanges[0].To != "+2348000000149" {
		t.Errorf("ranges changed when not asked to: %+v", c.DIDRanges)
	}

	// An explicit null leaves the metadata alone rather than storing a jsonb
	// null nobody can read keys from.
	c, err = pool.UpdateTrafficConfig(ctx, f.acmeInbound, "acme", TrafficConfigUpdate{
		Metadata: json.RawMessage(`null`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(c.Metadata) != `{"queue": "lagos"}` {
		t.Errorf("metadata = %s, want it kept", c.Metadata)
	}

	// Another tenant cannot touch it.
	if _, err := pool.UpdateTrafficConfig(ctx, f.acmeInbound, "beta", TrafficConfigUpdate{
		RecordingMode: &full,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("another tenant updated the line: %v", err)
	}
}

// Deleting a line takes its overrides with it: an override for a line that is
// gone can never be routed to, and leaving it listed invites someone to edit it.
func TestTrafficConfig_DeleteTakesItsOverrides(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	if _, err := pool.CreateHandoffTarget(ctx, HandoffTarget{
		TenantID: "acme", TrafficConfigID: &f.acmeInbound,
		Name: "Lagos", Kind: route.KindSIPURI, Destination: "sip:q@lagos", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.CreateHandoffTarget(ctx, HandoffTarget{
		TenantID: "acme", Name: "Default", Kind: route.KindSIPURI, Destination: "sip:q@hq", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	if err := pool.DeleteTrafficConfig(ctx, f.acmeInbound, "acme"); err != nil {
		t.Fatal(err)
	}

	list, err := pool.ListHandoffTargets(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "Default" {
		t.Errorf("want only the default target left, got %+v", list)
	}

	// The numbers are free again.
	if _, err := pool.CreateTrafficConfig(ctx, TrafficConfigCreate{
		TenantID: "beta", TrunkID: f.platform, Name: "Reuse", Direction: "inbound",
		DIDRanges: []numrange.Bounds{block("+2348000000100", "+2348000000149")}, Destination: "ai",
	}); err != nil {
		t.Errorf("a deleted line's numbers could not be reused: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Handoff resolution — the M7 gate
// ---------------------------------------------------------------------------

// Every combination of trunk and handoff kind must produce something
// dialable. The platform-trunk × E.164 row is the one that used to fail:
// handoff looked for "the tenant's default trunk", and a tenant on the
// platform trunk has none.
func TestHandoffTargets_EveryTrunkAndKindCombination(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	byotLine, err := pool.CreateTrafficConfig(ctx, TrafficConfigCreate{
		TenantID: "acme", TrunkID: f.byot, Name: "Support", Direction: "inbound",
		DIDRanges: []numrange.Bounds{block("+4930150000", "+4930150009")}, Destination: "handoff",
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		line     int64
		trunk    int64
		kind     string
		dest     string
		wantHost string
	}{
		{"platform trunk, SIP URI", f.acmeInbound, f.platform, route.KindSIPURI, "sip:queue@sbc.acme", "sip:queue@sbc.acme"},
		{"platform trunk, E.164", f.acmeInbound, f.platform, route.KindE164, "+2348099999999", "+2348099999999@x.pstn.twilio.com"},
		{"BYOT trunk, SIP URI", byotLine.ID, f.byot, route.KindSIPURI, "sip:queue@sbc.acme", "sip:queue@sbc.acme"},
		{"BYOT trunk, E.164", byotLine.ID, f.byot, route.KindE164, "+4930999999", "+4930999999@sbc.acme:5080"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mustExec(t, pool, `UPDATE handoff_targets SET deleted_at = now() WHERE deleted_at IS NULL`)
			if _, err := pool.CreateHandoffTarget(ctx, HandoffTarget{
				TenantID: "acme", Name: c.name, Kind: c.kind, Destination: c.dest, Enabled: true,
			}); err != nil {
				t.Fatal(err)
			}

			targets, err := pool.HandoffTargets(ctx, "acme", c.line, c.trunk)
			if err != nil {
				t.Fatal(err)
			}
			dial, err := route.Handoff(route.Params{CallUUID: "u", TenantID: "acme"}, targets)
			if err != nil {
				t.Fatalf("not dialable: %v (targets %+v)", err, targets)
			}
			if !strings.Contains(dial, c.wantHost) {
				t.Errorf("dial string %q should contain %q", dial, c.wantHost)
			}
		})
	}
}

// A line with an enabled override uses only its overrides; every other line
// uses the tenant default.
func TestHandoffTargets_OverridesReplaceTheDefault(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	create := func(name string, line *int64, enabled bool) {
		t.Helper()
		if _, err := pool.CreateHandoffTarget(ctx, HandoffTarget{
			TenantID: "acme", TrafficConfigID: line, Name: name,
			Kind: route.KindSIPURI, Destination: "sip:" + name + "@sbc", Enabled: enabled,
		}); err != nil {
			t.Fatal(err)
		}
	}

	create("hq", nil, true)

	names := func(line int64) []string {
		t.Helper()
		ts, err := pool.HandoffTargets(ctx, "acme", line, f.platform)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range ts {
			out = append(out, x.Destination)
		}
		return out
	}

	if got := names(f.acmeInbound); len(got) != 1 || got[0] != "sip:hq@sbc" {
		t.Errorf("with no override, want the default: %v", got)
	}

	// A DISABLED override does not count — the line falls back to the default
	// rather than to nothing.
	create("off", &f.acmeInbound, false)
	if got := names(f.acmeInbound); len(got) != 1 || got[0] != "sip:hq@sbc" {
		t.Errorf("a disabled override hid the default: %v", got)
	}

	create("lagos", &f.acmeInbound, true)
	if got := names(f.acmeInbound); len(got) != 1 || got[0] != "sip:lagos@sbc" {
		t.Errorf("with an override, want only it: %v", got)
	}

	// No line at all — a call with nothing but a tenant — gets the default.
	if got := names(0); len(got) != 1 || got[0] != "sip:hq@sbc" {
		t.Errorf("with no line, want the default: %v", got)
	}
}

// ---------------------------------------------------------------------------
// Coverage
// ---------------------------------------------------------------------------

func TestTrunkCoverage_ReportsTheGaps(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	if _, err := pool.CreateTrafficConfig(ctx, TrafficConfigCreate{
		TenantID: "beta", TrunkID: f.platform, Name: "Campaign", Direction: "outbound",
		DIDRanges: []numrange.Bounds{block("+2348000000180", "+2348000000199")}, Destination: "ai",
	}); err != nil {
		t.Fatal(err)
	}

	cov, err := pool.TrunkCoverage(ctx, f.platform)
	if err != nil {
		t.Fatal(err)
	}

	eq := func(got []numrange.Range, want ...numrange.Range) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range want {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	if !eq(cov.UncoveredInbound, numrange.Range{From: "+2348000000150", To: "+2348000000199"}) {
		t.Errorf("uncovered inbound = %+v", cov.UncoveredInbound)
	}
	if !eq(cov.UncoveredOutbound, numrange.Range{From: "+2348000000100", To: "+2348000000179"}) {
		t.Errorf("uncovered outbound = %+v", cov.UncoveredOutbound)
	}
	if len(cov.Inbound) != 1 || cov.Inbound[0].TenantID != "acme" {
		t.Errorf("inbound lines = %+v", cov.Inbound)
	}
	if len(cov.Outbound) != 1 || cov.Outbound[0].TenantID != "beta" {
		t.Errorf("outbound lines = %+v", cov.Outbound)
	}
}

// A trunk with no lines at all is entirely uncovered — and an empty result
// must not come back as null.
func TestTrunkCoverage_NoLines(t *testing.T) {
	pool := migratedPool(t)
	f := seedNumbers(t, pool)

	cov, err := pool.TrunkCoverage(context.Background(), f.byot)
	if err != nil {
		t.Fatal(err)
	}
	if len(cov.UncoveredInbound) != 1 || cov.UncoveredInbound[0].From != "+4930150000" {
		t.Errorf("uncovered inbound = %+v", cov.UncoveredInbound)
	}
	if cov.Inbound == nil || cov.Outbound == nil {
		t.Error("empty line lists must be [], not null")
	}
}

func TestTrunkCoverage_UnknownTrunk(t *testing.T) {
	pool := migratedPool(t)
	if _, err := pool.TrunkCoverage(context.Background(), 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got: %v", err)
	}
}
