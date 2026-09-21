// Package admission answers the question every inbound call starts with:
// does anyone own this number, and may this call proceed?
//
// # This is a package, not an endpoint
//
// It began as GET /v1/route, called by Kamailio on every inbound INVITE. That
// call was the single blocking dependency on the SIP path — Kamailio is
// process-per-message, so a worker waiting on HTTP handled nothing else, and
// inbound capacity was capped at roughly children ÷ timeout.
//
// Kamailio no longer asks. It accepts an INVITE from an allowed carrier range
// and proxies straight to FreeSWITCH; the controller resolves the number once
// it has the call. The work did not get cheaper, it moved somewhere it blocks
// a goroutine instead of a SIP worker, and somewhere it can be cached.
//
// Two callers, which is why this is shared code rather than two copies:
//
//	esl-controller   per call, in the path, over its own database pool.
//	control-plane    GET /v1/route, for an operator asking why a number is
//	                 not working. Same code, so the answer is the real one.
//
// # Caching
//
// Reads are cached in-process. They are operator-edited configuration —
// minutes to days between changes — and were otherwise re-read on every single
// call. The TTL is a staleness budget: an edit takes up to one TTL to reach a
// live call.
//
// This is not an optimisation. The database is a db.t4g.micro, and its own
// Terraform variable warns that the instance "stops being defensible if the
// API does not cache and every inbound INVITE becomes a query" — burstable
// instances do not fail under that load, they exhaust CPU credits and then
// throttle, which presents as call setup latency degrading over hours and
// recovering overnight.
//
// Serve-stale is the half that matters most. A database blip would otherwise
// fail every call that arrives during it; within the stale window, calls keep
// routing on the last known-good answer.
package admission

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Reason explains a verdict. It is recorded on the CDR and returned by the
// operator endpoint, so it is part of the interface rather than a log string.
type Reason string

const (
	ReasonOK              Reason = "ok"
	ReasonUnknownDID      Reason = "unknown_did"
	ReasonDIDInactive     Reason = "did_inactive"
	ReasonNoPlan          Reason = "no_plan"
	ReasonPlanInactive    Reason = "plan_inactive"
	ReasonTenantSuspended Reason = "tenant_suspended"
	ReasonPlanRejects     Reason = "plan_rejects"

	// ReasonOverCapacity is NOT produced here. The concurrency cap is enforced
	// by the conditional insert that claims a slot, because checking a count
	// and then inserting is a race two simultaneous calls can both win. It is
	// named here so the vocabulary lives in one place.
	ReasonOverCapacity Reason = "over_capacity"

	// ReasonLookupFailed means the database was unreachable and no cached
	// answer existed. The controller hangs up NORMAL_TEMPORARY_FAILURE.
	ReasonLookupFailed Reason = "lookup_failed"
)

// Destination is what to do with an admitted call.
type Destination string

const (
	DestinationAI      Destination = "ai"
	DestinationHandoff Destination = "handoff"
	DestinationReject  Destination = "reject"
)

// Verdict is the whole answer. It carries the plan, not two opaque ids: the
// controller needs the plan config regardless, and resolving it in the same
// pass is one query rather than two.
type Verdict struct {
	Admitted    bool
	Reason      Reason
	TenantID    string
	PlanID      string
	Destination Destination
	Recording   bool
	TrunkID     int64

	// Stale reports that this answer came from an expired cache entry because
	// the database could not be reached. The call proceeds; the log should say
	// so, because a platform running on stale config is working and not
	// healthy.
	Stale bool
}

// Route is the raw configuration a number resolves to, before any policy is
// applied. It is what gets cached.
type Route struct {
	Number       string
	TenantID     string
	TenantActive bool
	DIDActive    bool
	PlanID       string
	PlanActive   bool
	Destination  Destination
	Recording    bool
	TrunkID      int64
}

// ErrNotFound means no DID and no range matched. It is an ordinary outcome —
// most of the numbers a carrier can deliver are not ours — and is cached.
var ErrNotFound = errors.New("admission: no route for number")

// Lookup is the database half, defined here so this package stays a leaf and
// can be tested without Postgres. internal/store implements it.
type Lookup interface {
	ResolveNumber(ctx context.Context, normalised string) (Route, error)
}

// Service resolves numbers, with a cache in front.
type Service struct {
	lookup Lookup
	cache  *cache
	log    *slog.Logger
}

// Options tunes the cache. The defaults are deliberate; see cache.go.
type Options struct {
	TTL         time.Duration
	NegativeTTL time.Duration
	StaleWindow time.Duration
	MaxEntries  int
}

// New builds a Service.
func New(lookup Lookup, log *slog.Logger, opts Options) *Service {
	return &Service{
		lookup: lookup,
		cache:  newCache(opts),
		log:    log,
	}
}

// Resolve answers for one call.
//
// It never returns an error for a call that should simply be refused: an
// unowned number is the system working, not a fault, and a caller that has to
// distinguish "refused" from "broken" by inspecting an error will eventually
// get it wrong. The only error is a lookup that failed with nothing cached to
// fall back on.
func (s *Service) Resolve(ctx context.Context, did string) (Verdict, error) {
	number := Normalise(did)
	if number == "" {
		return Verdict{Admitted: false, Reason: ReasonUnknownDID}, nil
	}

	route, stale, err := s.get(ctx, number)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Verdict{Admitted: false, Reason: ReasonUnknownDID}, nil
		}
		return Verdict{Admitted: false, Reason: ReasonLookupFailed},
			fmt.Errorf("admission: resolving %s: %w", number, err)
	}

	if stale {
		// Worth a line every time rather than a counter. This is rare, and
		// when it happens the reason the platform is still working is not
		// otherwise visible anywhere.
		s.log.Warn("serving admission from stale cache; database unreachable",
			"number", number, "tenant", route.TenantID)
	}

	v := evaluate(route)
	v.Stale = stale
	return v, nil
}

// evaluate applies policy to a resolved route. Split out because it is pure —
// every branch here is a test that needs no database and no clock.
//
// Order matters, and it runs cheapest-and-most-specific first so the reason
// recorded on the CDR is the most useful one available. A suspended tenant
// whose plan is also inactive should read as suspended: that is the fact
// someone needs.
func evaluate(r Route) Verdict {
	v := Verdict{
		TenantID:    r.TenantID,
		PlanID:      r.PlanID,
		Destination: r.Destination,
		Recording:   r.Recording,
		TrunkID:     r.TrunkID,
	}

	switch {
	case !r.TenantActive:
		v.Reason = ReasonTenantSuspended
	case !r.DIDActive:
		v.Reason = ReasonDIDInactive
	case r.PlanID == "":
		// A DID that resolves to a tenant but no plan. The number is ours and
		// nobody has said what to do with it — distinct from an unknown
		// number, and the fix is different.
		v.Reason = ReasonNoPlan
	case !r.PlanActive:
		v.Reason = ReasonPlanInactive
	case r.Destination == DestinationReject:
		// Configured to refuse. Not a fault; someone chose this.
		v.Reason = ReasonPlanRejects
	default:
		v.Admitted = true
		v.Reason = ReasonOK
	}

	return v
}

// get returns a route from cache, or loads it.
func (s *Service) get(ctx context.Context, number string) (Route, bool, error) {
	if entry, ok := s.cache.fresh(number); ok {
		if entry.notFound {
			return Route{}, false, ErrNotFound
		}
		return entry.route, false, nil
	}

	route, err := s.lookup.ResolveNumber(ctx, number)
	switch {
	case err == nil:
		s.cache.put(number, route, false)
		return route, false, nil

	case errors.Is(err, ErrNotFound):
		// Cache the miss, and this matters more than it looks.
		//
		// Kamailio now proxies every INVITE from an allowed carrier range
		// straight through, so scanner traffic and misdialled numbers reach
		// this code. Without a negative cache each one is a fresh query, and a
		// burst of junk calls to numbers we do not own becomes database load
		// on the instance least able to absorb it.
		s.cache.put(number, Route{}, true)
		return Route{}, false, ErrNotFound

	default:
		// The database failed. Fall back to an expired entry if there is one:
		// a DID-to-tenant mapping changes on the order of days, so a
		// minutes-old answer is almost certainly still right, and the
		// alternative is refusing a call that would have worked.
		if entry, ok := s.cache.stale(number); ok {
			if entry.notFound {
				return Route{}, true, ErrNotFound
			}
			return entry.route, true, nil
		}
		return Route{}, false, err
	}
}

// Warm preloads the cache. Called at startup.
//
// A cold process after a deploy otherwise takes its first calls at database
// latency — and fails all of them if the database is the thing that is wrong,
// with no stale entry to fall back on. That is the worst possible moment for
// the cache to be empty.
func (s *Service) Warm(ctx context.Context, routes []Route) {
	for _, r := range routes {
		s.cache.put(r.Number, r, false)
	}
	s.log.Info("admission cache warmed", "routes", len(routes))
}

// CacheSize reports how many numbers are currently cached. For the startup log
// and the operator endpoint — "is the cache warm?" is the first question when
// call setup is slow, and it should not need a profiler to answer.
func (s *Service) CacheSize() int {
	return s.cache.len()
}

// Invalidate drops one number, for the control plane to call after an edit so
// its own view is immediately correct. It does NOT reach the controller's
// cache — the two are separate processes and each expires on its own TTL. An
// edit takes up to one TTL to reach a live call, which is the staleness budget
// this design accepts.
func (s *Service) Invalidate(number string) {
	s.cache.drop(Normalise(number))
}
