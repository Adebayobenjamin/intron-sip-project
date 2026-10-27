package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/numrange"
)

// ResolveNumber maps a normalised E.164 number to the configuration a call
// needs. This is the query on the inbound call path and the only one there, so
// it is one round trip.
//
// Two questions, answered in that order:
//
//	is it ours?               a trunk's did_ranges contains it
//	what happens when it rings  an inbound traffic_config on that trunk covers it
//
// The join to traffic_configs is LEFT because a number on a trunk with no line
// configured is still ours. That must resolve to "ours, nothing configured"
// rather than "not ours": the two need different fixes and only one of them is
// a mistake.
//
// At most one row can match. The schema refuses two trunks carrying the same
// number and two inbound lines claiming it, so LIMIT 1 is a guard rather than
// a choice between candidates.
func (p *Pool) ResolveNumber(ctx context.Context, number string) (admission.Route, error) {
	n, err := numrange.Value(number)
	if err != nil {
		// Too long for int8, a leading zero, no digits: not a number any trunk
		// can carry, so a miss rather than an error.
		return admission.Route{}, admission.ErrNotFound
	}

	r, err := scanRoute(p.QueryRow(ctx, resolveNumberSQL, n))
	if errors.Is(err, pgx.ErrNoRows) {
		return admission.Route{}, admission.ErrNotFound
	}
	if err != nil {
		return admission.Route{}, fmt.Errorf("store: resolving %s: %w", number, err)
	}

	r.Number = number
	return r, nil
}

// routeColumns and routeJoins are shared by the call path and the cache
// warm, so the two can never disagree about what a number means.
//
// The tenant comes from the line when there is one and from the trunk when
// there is not. On a client's own trunk an unconfigured number is still the
// client's; on the platform trunk it belongs to nobody until a line leases it.
const routeColumns = `
    t.id                                        AS trunk_id,
    COALESCE(tc.tenant_id, t.tenant_id, '')     AS tenant_id,
    COALESCE(ten.status = 'active', false)      AS tenant_active,
    COALESCE(tc.id, 0)                          AS traffic_config_id,
    COALESCE(tc.status = 'active', false)       AS config_active,
    COALESCE(tc.destination, '')                AS destination,
    COALESCE(tc.recording_mode, 'none')         AS recording_mode`

// routeJoins resolves the number in column q.n. Joins rather than a whole
// FROM, so the call path can supply one bound number and the warm a set of
// them.
const routeJoins = `
JOIN trunks t
  ON t.deleted_at IS NULL
 AND t.did_ranges @> q.n
LEFT JOIN traffic_configs tc
       ON tc.trunk_id = t.id
      AND tc.direction = 'inbound'
      AND tc.deleted_at IS NULL
      AND tc.did_ranges @> q.n
LEFT JOIN tenants ten
       ON ten.id = COALESCE(tc.tenant_id, t.tenant_id)
      AND ten.deleted_at IS NULL`

// resolveNumberSQL is the call-path query. The number is compared as int8,
// which is what the GiST indexes behind the two exclusion constraints answer.
const resolveNumberSQL = `
SELECT ` + routeColumns + `
  FROM (SELECT $1::int8 AS n) AS q` + routeJoins + `
 LIMIT 1`

func scanRoute(row pgx.Row) (admission.Route, error) {
	var r admission.Route
	var destination, mode string

	err := row.Scan(&r.TrunkID, &r.TenantID, &r.TenantActive,
		&r.TrafficConfigID, &r.ConfigActive, &destination, &mode)
	if err != nil {
		return admission.Route{}, err
	}

	r.Destination = admission.Destination(destination)
	r.RecordingMode = admission.RecordingMode(mode)
	return r, nil
}

// warmRangeLimit caps the size of a block whose numbers are enumerated to
// warm the cache.
//
// Postgres materialises a set-returning function in FROM before LIMIT applies,
// so generate_series over a ten-million-number block would build ten million
// rows to keep five thousand. Small blocks — a line of a hundred numbers — are
// enumerated; large ones resolve on demand and are cached as they are called.
const warmRangeLimit = 10_000

// ActiveRoutes loads the numbers behind inbound lines, for warming the cache
// at startup.
//
// The limit is a guard rather than a policy: a warm that quietly loads a
// million rows is a slow startup nobody attributes to this.
func (p *Pool) ActiveRoutes(ctx context.Context, limit int) ([]admission.Route, error) {
	rows, err := p.Query(ctx, `
		WITH q AS (
		    SELECT g.n
		      FROM traffic_configs c
		     CROSS JOIN LATERAL unnest(c.did_ranges) AS r
		     CROSS JOIN LATERAL generate_series(lower(r), upper(r) - 1) AS g(n)
		     WHERE c.direction = 'inbound'
		       AND c.deleted_at IS NULL
		       AND upper(r) - lower(r) <= $2
		     LIMIT $1
		)
		SELECT '+' || q.n::text, `+routeColumns+`
		  FROM q`+routeJoins,
		limit, warmRangeLimit)
	if err != nil {
		return nil, fmt.Errorf("store: loading routes to warm cache: %w", err)
	}
	defer rows.Close()

	var out []admission.Route
	for rows.Next() {
		var r admission.Route
		var destination, mode string
		if err := rows.Scan(&r.Number, &r.TrunkID, &r.TenantID, &r.TenantActive,
			&r.TrafficConfigID, &r.ConfigActive, &destination, &mode); err != nil {
			return nil, fmt.Errorf("store: scanning route: %w", err)
		}
		r.Destination = admission.Destination(destination)
		r.RecordingMode = admission.RecordingMode(mode)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// GET /v1/numbers/{e164}
// ---------------------------------------------------------------------------

// NumberInfo is everything the platform knows about one number: which trunk
// carries it, and which line handles it in each direction.
//
// The diagnostic an operator reaches for before /v1/route. /v1/route answers
// "would a call be admitted right now"; this answers "who does this number
// belong to and what is configured on it", including the outbound side that
// /v1/route never looks at.
type NumberInfo struct {
	Number   string         `json:"number"`
	Trunk    NumberTrunk    `json:"trunk"`
	Inbound  *TrafficConfig `json:"inbound,omitempty"`
	Outbound *TrafficConfig `json:"outbound,omitempty"`
}

// NumberTrunk is the trunk side of NumberInfo — enough to identify it, without
// the connection details a tenant leasing a platform number has no use for.
type NumberTrunk struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug"`
	TenantID string `json:"tenant_id,omitempty"`
	Status   string `json:"status"`

	// Platform is true for the shared trunk, whose numbers are leased to
	// tenants line by line.
	Platform bool `json:"platform"`
}

// Number looks one number up.
//
// Tenant-scoped callers see a number only if it is theirs — on their own
// trunk, or leased to them by a line — and see only their own lines on it. A
// number on the platform trunk can be leased inbound to one tenant and
// outbound to another; neither should learn about the other.
func (p *Pool) Number(ctx context.Context, number, tenantID string) (NumberInfo, error) {
	n, err := numrange.Value(number)
	if err != nil {
		return NumberInfo{}, ErrNotFound
	}

	info := NumberInfo{Number: number}
	var trunkTenant *string

	err = p.QueryRow(ctx, `
		SELECT id, slug, tenant_id, status
		  FROM trunks
		 WHERE deleted_at IS NULL AND did_ranges @> $1::int8`, n,
	).Scan(&info.Trunk.ID, &info.Trunk.Slug, &trunkTenant, &info.Trunk.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return NumberInfo{}, ErrNotFound
	}
	if err != nil {
		return NumberInfo{}, fmt.Errorf("store: looking up number %s: %w", number, err)
	}

	info.Trunk.TenantID = deref(trunkTenant)
	info.Trunk.Platform = trunkTenant == nil

	rows, err := p.Query(ctx, `
		SELECT `+trafficConfigColumns+`
		  FROM traffic_configs c
		 WHERE c.deleted_at IS NULL
		   AND c.trunk_id = $1
		   AND c.did_ranges @> $2::int8
		   AND ($3::text IS NULL OR c.tenant_id = $3)`,
		info.Trunk.ID, n, nullStringArg(tenantID))
	if err != nil {
		return NumberInfo{}, fmt.Errorf("store: lines for number %s: %w", number, err)
	}
	defer rows.Close()

	for rows.Next() {
		c, err := scanTrafficConfig(rows)
		if err != nil {
			return NumberInfo{}, fmt.Errorf("store: scanning line: %w", err)
		}
		switch c.Direction {
		case "inbound":
			info.Inbound = &c
		case "outbound":
			info.Outbound = &c
		}
	}
	if err := rows.Err(); err != nil {
		return NumberInfo{}, err
	}

	// Not theirs: neither on their trunk nor leased to them. Indistinguishable
	// from a number nobody owns, for the reason given on ErrNotFound.
	if tenantID != "" && info.Trunk.TenantID != tenantID &&
		info.Inbound == nil && info.Outbound == nil {
		return NumberInfo{}, ErrNotFound
	}

	return info, nil
}
