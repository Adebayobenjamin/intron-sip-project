package admission

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeLookup stands in for the database. Counting calls is the point of half
// these tests — the cache exists so the database is not asked twice.
type fakeLookup struct {
	routes map[string]Route
	err    error
	calls  int
}

func (f *fakeLookup) ResolveNumber(_ context.Context, number string) (Route, error) {
	f.calls++
	if f.err != nil {
		return Route{}, f.err
	}
	r, ok := f.routes[number]
	if !ok {
		return Route{}, ErrNotFound
	}
	return r, nil
}

func liveRoute() Route {
	return Route{
		Number:          "+4930123456",
		TenantID:        "t1",
		TenantActive:    true,
		TrafficConfigID: 11,
		ConfigActive:    true,
		Destination:     DestinationAI,
		RecordingMode:   RecordingAIOnly,
		TrunkID:         7,
	}
}

func newService(l Lookup) *Service {
	return New(l, quiet(), Options{})
}

// ---------------------------------------------------------------------------
// Normalisation
// ---------------------------------------------------------------------------

func TestNormalise(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already E.164", "+4930123456", "+4930123456"},
		{"bare international", "4930123456", "+4930123456"},
		{"ITU access prefix", "004930123456", "+4930123456"},
		{"carrier formatting", "+49 (30) 123-456", "+4930123456"},
		{"dots", "+49.30.123.456", "+4930123456"},
		{"empty", "", ""},
		{"no digits", "anonymous", ""},
		{"only the access prefix", "00", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalise(tc.in); got != tc.want {
				t.Errorf("Normalise(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Every shape of the same number must reach one cache entry and one lookup.
// Otherwise the same caller dialling the same number twice could get two
// different answers.
func TestNormalise_ShapesConverge(t *testing.T) {
	want := Normalise("+4930123456")
	for _, in := range []string{"4930123456", "004930123456", "+49 30 123 456"} {
		if got := Normalise(in); got != want {
			t.Errorf("Normalise(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Policy — pure, no database, no clock
// ---------------------------------------------------------------------------

func TestEvaluate(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*Route)
		wantAdmit  bool
		wantReason Reason
	}{
		{
			name:       "fully configured",
			mutate:     func(*Route) {},
			wantAdmit:  true,
			wantReason: ReasonOK,
		},
		{
			name:       "suspended tenant",
			mutate:     func(r *Route) { r.TenantActive = false },
			wantReason: ReasonTenantSuspended,
		},
		{
			// A trunk carries the number and no line covers it. Ours, and
			// nobody said what to do with it — a different problem from an
			// unknown number, with a different fix.
			name:       "no line configured",
			mutate:     func(r *Route) { r.TrafficConfigID = 0; r.ConfigActive = false },
			wantReason: ReasonNoTrafficConfig,
		},
		{
			// The platform trunk with no line: no tenant at all yet. Must not
			// read as a suspended tenant, which would name a tenant that does
			// not exist.
			name: "platform number with no line and no tenant",
			mutate: func(r *Route) {
				r.TenantID, r.TenantActive = "", false
				r.TrafficConfigID, r.ConfigActive = 0, false
			},
			wantReason: ReasonNoTrafficConfig,
		},
		{
			name:       "line switched off",
			mutate:     func(r *Route) { r.ConfigActive = false },
			wantReason: ReasonTrafficConfigInactive,
		},
		{
			name:       "line configured to refuse",
			mutate:     func(r *Route) { r.Destination = DestinationReject },
			wantReason: ReasonTrafficConfigRejects,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := liveRoute()
			tc.mutate(&r)

			v := evaluate(r)
			if v.Admitted != tc.wantAdmit {
				t.Errorf("Admitted = %v, want %v", v.Admitted, tc.wantAdmit)
			}
			if v.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", v.Reason, tc.wantReason)
			}
		})
	}
}

// A suspended tenant whose line is also inactive must read as suspended. That
// is the fact someone needs; "line inactive" would send them to the wrong
// screen.
func TestEvaluate_MostUsefulReasonWins(t *testing.T) {
	r := liveRoute()
	r.TenantActive = false
	r.ConfigActive = false

	if got := evaluate(r).Reason; got != ReasonTenantSuspended {
		t.Errorf("Reason = %q, want %q", got, ReasonTenantSuspended)
	}
}

// An admitted verdict must carry the line, not just ids. The controller needs
// it, and a second query per call is what this avoids.
func TestResolve_CarriesTheLine(t *testing.T) {
	f := &fakeLookup{routes: map[string]Route{"+4930123456": liveRoute()}}

	v, err := newService(f).Resolve(context.Background(), "+4930123456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Admitted {
		t.Fatalf("want admitted, got %q", v.Reason)
	}
	if v.Destination != DestinationAI || v.RecordingMode != RecordingAIOnly ||
		v.TrunkID != 7 || v.TrafficConfigID != 11 {
		t.Errorf("line not carried through: %+v", v)
	}
}

// ---------------------------------------------------------------------------
// Refusals are not errors
// ---------------------------------------------------------------------------

// An unowned number is the system working. A caller forced to tell "refused"
// from "broken" by inspecting an error will eventually get it wrong, and the
// two need opposite SIP responses: 404 versus 503.
func TestResolve_UnknownNumberIsNotAnError(t *testing.T) {
	f := &fakeLookup{routes: map[string]Route{}}

	v, err := newService(f).Resolve(context.Background(), "+4930999999")
	if err != nil {
		t.Fatalf("an unowned number must not be an error, got: %v", err)
	}
	if v.Admitted {
		t.Error("want refused")
	}
	if v.Reason != ReasonUnknownDID {
		t.Errorf("Reason = %q, want %q", v.Reason, ReasonUnknownDID)
	}
}

func TestResolve_UnparseableNumberIsRefused(t *testing.T) {
	f := &fakeLookup{routes: map[string]Route{}}

	v, err := newService(f).Resolve(context.Background(), "anonymous")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Admitted || v.Reason != ReasonUnknownDID {
		t.Errorf("want refused as unknown, got admitted=%v reason=%q", v.Admitted, v.Reason)
	}
	if f.calls != 0 {
		t.Errorf("a number with no digits should not reach the database, got %d calls", f.calls)
	}
}

// ---------------------------------------------------------------------------
// Caching
// ---------------------------------------------------------------------------

func TestResolve_CachesPositiveAnswers(t *testing.T) {
	f := &fakeLookup{routes: map[string]Route{"+4930123456": liveRoute()}}
	s := newService(f)

	for range 5 {
		if _, err := s.Resolve(context.Background(), "+4930123456"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if f.calls != 1 {
		t.Errorf("want 1 database call for 5 resolves, got %d", f.calls)
	}
}

// Kamailio now proxies every INVITE from an allowed carrier range straight
// through, so scanner traffic reaches this code. Without a negative cache each
// junk call is a fresh query against the instance least able to absorb it.
func TestResolve_CachesMisses(t *testing.T) {
	f := &fakeLookup{routes: map[string]Route{}}
	s := newService(f)

	for range 10 {
		if _, err := s.Resolve(context.Background(), "+4930999999"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if f.calls != 1 {
		t.Errorf("want 1 database call for 10 misses, got %d", f.calls)
	}
}

// A miss must expire sooner than a hit: provisioning a number is exactly when
// someone is watching and testing it.
func TestCache_NegativeTTLIsShorterThanPositive(t *testing.T) {
	c := newCache(Options{})
	if c.negativeTTL >= c.ttl {
		t.Errorf("negative TTL %s should be shorter than positive TTL %s", c.negativeTTL, c.ttl)
	}
}

func TestResolve_RefetchesAfterTTL(t *testing.T) {
	f := &fakeLookup{routes: map[string]Route{"+4930123456": liveRoute()}}
	s := New(f, quiet(), Options{TTL: time.Minute})

	clock := time.Now()
	s.cache.now = func() time.Time { return clock }

	if _, err := s.Resolve(context.Background(), "+4930123456"); err != nil {
		t.Fatal(err)
	}

	clock = clock.Add(2 * time.Minute)

	if _, err := s.Resolve(context.Background(), "+4930123456"); err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 {
		t.Errorf("want a refetch once the TTL passed, got %d calls", f.calls)
	}
}

// ---------------------------------------------------------------------------
// Serve-stale — the behaviour the cache mostly exists for
// ---------------------------------------------------------------------------

// A database blip would otherwise fail every call that arrives during it. A
// DID-to-tenant mapping changes on the order of days, so a minutes-old answer
// is almost certainly still right.
func TestResolve_ServesStaleWhenDatabaseIsDown(t *testing.T) {
	f := &fakeLookup{routes: map[string]Route{"+4930123456": liveRoute()}}
	s := New(f, quiet(), Options{TTL: time.Minute, StaleWindow: 15 * time.Minute})

	clock := time.Now()
	s.cache.now = func() time.Time { return clock }

	if _, err := s.Resolve(context.Background(), "+4930123456"); err != nil {
		t.Fatal(err)
	}

	// Entry expires, then the database goes away.
	clock = clock.Add(2 * time.Minute)
	f.err = errors.New("connection refused")

	v, err := s.Resolve(context.Background(), "+4930123456")
	if err != nil {
		t.Fatalf("want the stale entry served, got error: %v", err)
	}
	if !v.Admitted {
		t.Errorf("want the call admitted from stale config, got %q", v.Reason)
	}
	if !v.Stale {
		t.Error("want Stale set — running on stale config is working, not healthy")
	}
}

// Past the stale window, refusing is more honest than routing on configuration
// that may have been changed twice since.
func TestResolve_StopsServingStaleEventually(t *testing.T) {
	f := &fakeLookup{routes: map[string]Route{"+4930123456": liveRoute()}}
	s := New(f, quiet(), Options{TTL: time.Minute, StaleWindow: 5 * time.Minute})

	clock := time.Now()
	s.cache.now = func() time.Time { return clock }

	if _, err := s.Resolve(context.Background(), "+4930123456"); err != nil {
		t.Fatal(err)
	}

	clock = clock.Add(time.Hour)
	f.err = errors.New("connection refused")

	v, err := s.Resolve(context.Background(), "+4930123456")
	if err == nil {
		t.Fatal("want an error once the stale window has passed")
	}
	if v.Reason != ReasonLookupFailed {
		t.Errorf("Reason = %q, want %q", v.Reason, ReasonLookupFailed)
	}
}

// With nothing cached there is nothing to fall back on, and a database failure
// must be reported as one — the controller hangs up NORMAL_TEMPORARY_FAILURE
// rather than telling the carrier the number does not exist.
func TestResolve_ColdCacheFailsLoudly(t *testing.T) {
	f := &fakeLookup{err: errors.New("connection refused")}

	v, err := newService(f).Resolve(context.Background(), "+4930123456")
	if err == nil {
		t.Fatal("want an error with a cold cache and a failed lookup")
	}
	if v.Reason != ReasonLookupFailed {
		t.Errorf("Reason = %q, want %q — an outage must not look like an unknown number",
			v.Reason, ReasonLookupFailed)
	}
}

// ---------------------------------------------------------------------------
// Warming and invalidation
// ---------------------------------------------------------------------------

// A cold process after a deploy otherwise takes its first calls at database
// latency, and fails all of them if the database is the thing that is wrong.
func TestWarm_PopulatesTheCache(t *testing.T) {
	f := &fakeLookup{routes: map[string]Route{}}
	s := newService(f)

	s.Warm(context.Background(), []Route{liveRoute()})

	v, err := s.Resolve(context.Background(), "+4930123456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Admitted {
		t.Errorf("want admitted from the warmed entry, got %q", v.Reason)
	}
	if f.calls != 0 {
		t.Errorf("a warmed number should not hit the database, got %d calls", f.calls)
	}
}

func TestInvalidate_ForcesARefetch(t *testing.T) {
	f := &fakeLookup{routes: map[string]Route{"+4930123456": liveRoute()}}
	s := newService(f)

	if _, err := s.Resolve(context.Background(), "+4930123456"); err != nil {
		t.Fatal(err)
	}

	// Invalidation accepts any shape of the number, because a caller holding
	// one from an API request has not necessarily normalised it.
	s.Invalidate("004930123456")

	if _, err := s.Resolve(context.Background(), "+4930123456"); err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 {
		t.Errorf("want a refetch after invalidation, got %d calls", f.calls)
	}
}

// ---------------------------------------------------------------------------
// Memory bound
// ---------------------------------------------------------------------------

// Every number a carrier can deliver can reach this cache, not only the ones
// we own — so an unbounded map is a memory leak driven by whoever is scanning
// us today.
func TestCache_IsBounded(t *testing.T) {
	c := newCache(Options{MaxEntries: 10})

	for i := range 100 {
		c.put("+49301234"+string(rune('0'+i%10))+string(rune('0'+i/10)), Route{}, false)
	}

	if c.len() > 10 {
		t.Errorf("cache grew to %d entries with a bound of 10", c.len())
	}
}

// Refreshing a key already present is not growth, and must not evict anything.
func TestCache_RefreshIsNotGrowth(t *testing.T) {
	c := newCache(Options{MaxEntries: 2})

	c.put("+111", Route{TenantID: "a"}, false)
	c.put("+222", Route{TenantID: "b"}, false)
	c.put("+111", Route{TenantID: "a"}, false)

	if c.len() != 2 {
		t.Fatalf("want 2 entries, got %d", c.len())
	}
	if _, ok := c.fresh("+222"); !ok {
		t.Error("refreshing +111 should not have evicted +222")
	}
}
