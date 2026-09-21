package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
)

// ResolveNumber maps a normalised E.164 number to the configuration a call
// needs. This is the query on the inbound call path and the only one there, so
// it is one round trip.
//
// Two sources, checked in order:
//
//	dids        an individually provisioned number
//	did_ranges  a block bought at once
//
// The individual row wins. A number carved out of a block and pointed
// somewhere specific is the whole reason both tables exist, and the specific
// record has to beat the general one or the carve-out silently does nothing.
func (p *Pool) ResolveNumber(ctx context.Context, number string) (admission.Route, error) {
	route, err := p.resolveExactDID(ctx, number)
	if err == nil {
		return route, nil
	}
	if !errors.Is(err, admission.ErrNotFound) {
		return admission.Route{}, err
	}

	return p.resolveDIDRange(ctx, number)
}

// The joins are LEFT because a DID may legitimately have no plan yet — a
// number provisioned but not configured. That must resolve to "ours, no plan"
// rather than to "not ours", because the two need different fixes and only one
// of them is a mistake.
const resolveDIDSQL = `
SELECT
    d.number,
    d.tenant_id,
    t.status = 'active'                       AS tenant_active,
    d.status = 'active'                       AS did_active,
    COALESCE(p.id, '')                        AS plan_id,
    COALESCE(p.status = 'active', false)      AS plan_active,
    COALESCE(p.destination, '')               AS destination,
    COALESCE(p.recording, false)              AS recording,
    COALESCE(d.trunk_id, 0)                   AS trunk_id
FROM dids d
JOIN tenants t
  ON t.id = d.tenant_id AND t.deleted_at IS NULL
LEFT JOIN inbound_plans p
  ON p.id = d.plan_id AND p.deleted_at IS NULL
WHERE d.number = $1
  AND d.deleted_at IS NULL
LIMIT 1`

func (p *Pool) resolveExactDID(ctx context.Context, number string) (admission.Route, error) {
	var r admission.Route
	var destination string

	err := p.QueryRow(ctx, resolveDIDSQL, number).Scan(
		&r.Number,
		&r.TenantID,
		&r.TenantActive,
		&r.DIDActive,
		&r.PlanID,
		&r.PlanActive,
		&destination,
		&r.Recording,
		&r.TrunkID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return admission.Route{}, admission.ErrNotFound
	}
	if err != nil {
		return admission.Route{}, fmt.Errorf("store: resolving did %s: %w", number, err)
	}

	r.Destination = admission.Destination(destination)
	return r, nil
}

// did_ranges stores the numeric form, because a range needs numbers. The
// leading '+' is stripped for the comparison and put back on the way out, so
// callers only ever see one shape.
const resolveRangeSQL = `
SELECT
    r.tenant_id,
    t.status = 'active'                       AS tenant_active,
    r.status = 'active'                       AS range_active,
    COALESCE(p.id, '')                        AS plan_id,
    COALESCE(p.status = 'active', false)      AS plan_active,
    COALESCE(p.destination, '')               AS destination,
    COALESCE(p.recording, false)              AS recording,
    COALESCE(r.trunk_id, 0)                   AS trunk_id
FROM did_ranges r
JOIN tenants t
  ON t.id = r.tenant_id AND t.deleted_at IS NULL
LEFT JOIN inbound_plans p
  ON p.id = r.plan_id AND p.deleted_at IS NULL
WHERE r.number_range @> $1::bigint
  AND r.deleted_at IS NULL
ORDER BY upper(r.number_range) - lower(r.number_range) ASC
LIMIT 1`

func (p *Pool) resolveDIDRange(ctx context.Context, number string) (admission.Route, error) {
	digits := admission.Digits(number)

	// A number too long for int8 cannot be in any range this schema can hold,
	// so this is a miss rather than an error. E.164 caps at 15 digits, which
	// fits; anything longer is malformed and will not match a DID either.
	n, convErr := strconv.ParseInt(digits, 10, 64)
	if convErr != nil {
		return admission.Route{}, admission.ErrNotFound
	}

	var r admission.Route
	var destination string
	var rangeActive bool

	err := p.QueryRow(ctx, resolveRangeSQL, n).Scan(
		&r.TenantID,
		&r.TenantActive,
		&rangeActive,
		&r.PlanID,
		&r.PlanActive,
		&destination,
		&r.Recording,
		&r.TrunkID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return admission.Route{}, admission.ErrNotFound
	}
	if err != nil {
		return admission.Route{}, fmt.Errorf("store: resolving range for %s: %w", number, err)
	}

	r.Number = number
	r.DIDActive = rangeActive
	r.Destination = admission.Destination(destination)
	return r, nil
}

// ActiveRoutes loads every provisioned number, for warming the cache at
// startup.
//
// Individually provisioned DIDs only — ranges are not enumerated, because a
// range can cover ten thousand numbers and materialising them to fill a cache
// bounded at ten thousand entries would evict everything else to hold numbers
// nobody has called. Ranges are resolved on demand and cached as they are hit.
//
// The limit is a guard rather than a policy: a warm that quietly loads a
// million rows is a slow startup nobody attributes to this.
func (p *Pool) ActiveRoutes(ctx context.Context, limit int) ([]admission.Route, error) {
	const sql = resolveDIDAllSQL

	rows, err := p.Query(ctx, sql, limit)
	if err != nil {
		return nil, fmt.Errorf("store: loading routes to warm cache: %w", err)
	}
	defer rows.Close()

	var out []admission.Route
	for rows.Next() {
		var r admission.Route
		var destination string

		if err := rows.Scan(
			&r.Number,
			&r.TenantID,
			&r.TenantActive,
			&r.DIDActive,
			&r.PlanID,
			&r.PlanActive,
			&destination,
			&r.Recording,
			&r.TrunkID,
		); err != nil {
			return nil, fmt.Errorf("store: scanning route: %w", err)
		}

		r.Destination = admission.Destination(destination)
		out = append(out, r)
	}

	return out, rows.Err()
}

const resolveDIDAllSQL = `
SELECT
    d.number,
    d.tenant_id,
    t.status = 'active'                       AS tenant_active,
    d.status = 'active'                       AS did_active,
    COALESCE(p.id, '')                        AS plan_id,
    COALESCE(p.status = 'active', false)      AS plan_active,
    COALESCE(p.destination, '')               AS destination,
    COALESCE(p.recording, false)              AS recording,
    COALESCE(d.trunk_id, 0)                   AS trunk_id
FROM dids d
JOIN tenants t
  ON t.id = d.tenant_id AND t.deleted_at IS NULL
LEFT JOIN inbound_plans p
  ON p.id = d.plan_id AND p.deleted_at IS NULL
WHERE d.deleted_at IS NULL
ORDER BY d.number
LIMIT $1`
