package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/numrange"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/route"
)

// TrafficConfig is one line: a set of a trunk's numbers, a direction, and what
// happens to calls on them.
type TrafficConfig struct {
	ID            int64            `json:"id"`
	TenantID      string           `json:"tenant_id"`
	TrunkID       int64            `json:"trunk_id"`
	Name          string           `json:"name"`
	Direction     string           `json:"direction"`
	DIDRanges     []numrange.Range `json:"did_ranges"`
	Status        string           `json:"status"`
	Destination   string           `json:"destination"`
	RecordingMode string           `json:"recording_mode"`
	Metadata      json.RawMessage  `json:"metadata"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

// trafficConfigColumns expects the table aliased as c.
var trafficConfigColumns = `
	c.id, c.tenant_id, c.trunk_id, c.name, c.direction,
	` + rangeArraysSQL("c.did_ranges") + `,
	c.status, c.destination, c.recording_mode, c.metadata,
	c.created_at, c.updated_at`

func scanTrafficConfig(row pgx.Row) (TrafficConfig, error) {
	var c TrafficConfig
	var lowers, uppers []int64
	var meta []byte

	err := row.Scan(&c.ID, &c.TenantID, &c.TrunkID, &c.Name, &c.Direction,
		&lowers, &uppers,
		&c.Status, &c.Destination, &c.RecordingMode, &meta,
		&c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return TrafficConfig{}, err
	}

	if c.DIDRanges, err = rangesFrom(lowers, uppers); err != nil {
		return TrafficConfig{}, err
	}
	c.Metadata = json.RawMessage(meta)
	return c, nil
}

// TrafficConfigCreate is a new line. Ranges arrive already validated.
type TrafficConfigCreate struct {
	TenantID      string
	TrunkID       int64
	Name          string
	Direction     string
	DIDRanges     []numrange.Bounds
	Status        string
	Destination   string
	RecordingMode string
	Metadata      json.RawMessage
}

// CreateTrafficConfig stores a line.
//
// The rules that matter are the database's, not this function's: the overlap
// constraint refuses a second line claiming the same numbers, and the trigger
// refuses numbers outside the trunk or a trunk belonging to another tenant.
// Both surface as ConflictError with a message the API can return.
func (p *Pool) CreateTrafficConfig(ctx context.Context, in TrafficConfigCreate) (TrafficConfig, error) {
	lowers, uppers := numrange.Columns(in.DIDRanges)

	row := p.QueryRow(ctx, `
		INSERT INTO traffic_configs AS c (
			tenant_id, trunk_id, name, direction, did_ranges,
			status, destination, recording_mode, metadata
		) VALUES (
			$1, $2, $3, $4, `+rangeFromArraysSQL("$5", "$6")+`,
			COALESCE(NULLIF($7, ''), 'active'), $8,
			COALESCE(NULLIF($9, ''), 'none'), COALESCE($10::jsonb, '{}'::jsonb)
		)
		RETURNING `+trafficConfigColumns,
		in.TenantID, in.TrunkID, in.Name, in.Direction, lowers, uppers,
		in.Status, in.Destination, in.RecordingMode, nullJSON(in.Metadata))

	c, err := scanTrafficConfig(row)
	if err != nil {
		return TrafficConfig{}, fmt.Errorf("store: creating traffic config: %w", classify(err))
	}
	return c, nil
}

// ListTrafficConfigs returns a tenant's lines, or every line for a platform
// caller. trunkID, when non-zero, narrows to one trunk.
func (p *Pool) ListTrafficConfigs(ctx context.Context, tenantID string, trunkID int64) ([]TrafficConfig, error) {
	rows, err := p.Query(ctx, `
		SELECT `+trafficConfigColumns+`
		  FROM traffic_configs c
		 WHERE c.deleted_at IS NULL
		   AND ($1::text IS NULL OR c.tenant_id = $1)
		   AND ($2::bigint IS NULL OR c.trunk_id = $2)
		 ORDER BY c.tenant_id, c.trunk_id, c.direction, c.id`,
		nullStringArg(tenantID), nullInt64Arg(trunkID))
	if err != nil {
		return nil, fmt.Errorf("store: listing traffic configs: %w", err)
	}
	defer rows.Close()

	out := []TrafficConfig{}
	for rows.Next() {
		c, err := scanTrafficConfig(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scanning traffic config: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetTrafficConfig reads one line.
func (p *Pool) GetTrafficConfig(ctx context.Context, id int64, tenantID string) (TrafficConfig, error) {
	c, err := scanTrafficConfig(p.QueryRow(ctx, `
		SELECT `+trafficConfigColumns+`
		  FROM traffic_configs c
		 WHERE c.id = $1 AND c.deleted_at IS NULL
		   AND ($2::text IS NULL OR c.tenant_id = $2)`,
		id, nullStringArg(tenantID)))
	if errors.Is(err, pgx.ErrNoRows) {
		return TrafficConfig{}, ErrNotFound
	}
	if err != nil {
		return TrafficConfig{}, fmt.Errorf("store: getting traffic config: %w", err)
	}
	return c, nil
}

// TrafficConfigUpdate is the patchable subset.
//
// trunk_id and direction are NOT here. Moving a line to another trunk or
// turning inbound numbers into outbound ones is a different line, and doing
// it in place would reroute live traffic mid-edit. Delete and recreate.
type TrafficConfigUpdate struct {
	Name          *string
	Status        *string
	Destination   *string
	RecordingMode *string
	Metadata      json.RawMessage

	// DIDRanges nil leaves the numbers alone. A pointer, because an empty
	// list is a request — and one the schema refuses — rather than "unchanged".
	DIDRanges *[]numrange.Bounds
}

// UpdateTrafficConfig applies a partial update.
func (p *Pool) UpdateTrafficConfig(ctx context.Context, id int64, tenantID string, u TrafficConfigUpdate) (TrafficConfig, error) {
	var lowers, uppers []int64
	if u.DIDRanges != nil {
		lowers, uppers = numrange.Columns(*u.DIDRanges)
	}

	row := p.QueryRow(ctx, `
		UPDATE traffic_configs AS c
		   SET name           = COALESCE($3, c.name),
		       status         = COALESCE($4, c.status),
		       destination    = COALESCE($5, c.destination),
		       recording_mode = COALESCE($6, c.recording_mode),
		       metadata       = COALESCE($7::jsonb, c.metadata),
		       did_ranges     = CASE WHEN $8::int8[] IS NULL THEN c.did_ranges
		                             ELSE `+rangeFromArraysSQL("$8", "$9")+` END,
		       updated_at     = now()
		 WHERE c.id = $1 AND c.deleted_at IS NULL
		   AND ($2::text IS NULL OR c.tenant_id = $2)
		RETURNING `+trafficConfigColumns,
		id, nullStringArg(tenantID), u.Name, u.Status, u.Destination, u.RecordingMode,
		nullJSON(u.Metadata), boundsArg(u.DIDRanges, lowers), boundsArg(u.DIDRanges, uppers))

	c, err := scanTrafficConfig(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return TrafficConfig{}, ErrNotFound
	}
	if err != nil {
		return TrafficConfig{}, fmt.Errorf("store: updating traffic config: %w", classify(err))
	}
	return c, nil
}

// DeleteTrafficConfig soft-deletes a line, and its handoff overrides with it.
//
// The overrides go in the same transaction. An override whose line is gone is
// dead configuration — nothing can ever route to it — and leaving it listed
// invites someone to edit it believing it does something.
//
// A live call on the line is unaffected: it holds its own snapshot of what it
// needs, and the deleted row is still there for its foreign key.
func (p *Pool) DeleteTrafficConfig(ctx context.Context, id int64, tenantID string) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: beginning delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE traffic_configs SET deleted_at = now(), updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL AND ($2::text IS NULL OR tenant_id = $2)`,
		id, nullStringArg(tenantID))
	if err != nil {
		return fmt.Errorf("store: deleting traffic config: %w", classify(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	if _, err := tx.Exec(ctx, `
		UPDATE handoff_targets SET deleted_at = now(), updated_at = now()
		 WHERE traffic_config_id = $1 AND deleted_at IS NULL`, id); err != nil {
		return fmt.Errorf("store: deleting overrides for traffic config %d: %w", id, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: committing delete: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The call path's view of a line
// ---------------------------------------------------------------------------

// TrafficConfigByID reads an active line for an OUTBOUND call that has just
// been answered.
//
// An outbound call has no dialled number of ours to resolve — the number is
// the customer's — so the line is named by the originate rather than found
// from a number.
func (p *Pool) TrafficConfigByID(ctx context.Context, id int64) (route.Line, error) {
	var l route.Line

	err := p.QueryRow(ctx, `
		SELECT id, tenant_id, trunk_id, destination, recording_mode
		  FROM traffic_configs
		 WHERE id = $1 AND status = 'active' AND deleted_at IS NULL`, id,
	).Scan(&l.ID, &l.TenantID, &l.TrunkID, &l.Destination, &l.RecordingMode)

	if errors.Is(err, pgx.ErrNoRows) {
		return route.Line{}, ErrNotFound
	}
	if err != nil {
		return route.Line{}, fmt.Errorf("store: getting traffic config %d: %w", id, err)
	}
	return l, nil
}

// ResolveOutbound finds the line an outbound call presenting `number` belongs
// to, and the trunk it leaves by.
//
// ONE LOOKUP, THREE ANSWERS: whether the tenant may present this number
// (authorisation), what the call presents (caller ID), and which trunk it
// leaves by. Deriving the trunk from the line makes a caller-ID/trunk mismatch
// unrepresentable — carriers verify the calling number belongs to the account
// the call arrives on, so presenting one tenant's number over another's trunk
// is a 403.
//
// Only ACTIVE outbound lines count. An inactive line is someone saying these
// numbers must not dial out right now.
func (p *Pool) ResolveOutbound(ctx context.Context, number, tenantID string) (calls.OutboundLine, error) {
	n, err := numrange.Value(number)
	if err != nil {
		return calls.OutboundLine{}, ErrNotFound
	}

	var l calls.OutboundLine
	err = p.QueryRow(ctx, `
		SELECT c.id, c.trunk_id, COALESCE(t.sip_host, ''), c.recording_mode
		  FROM traffic_configs c
		  JOIN trunks t ON t.id = c.trunk_id AND t.deleted_at IS NULL
		 WHERE c.direction = 'outbound'
		   AND c.status = 'active'
		   AND c.deleted_at IS NULL
		   AND c.tenant_id = $2
		   AND c.did_ranges @> $1::int8`, n, tenantID,
	).Scan(&l.TrafficConfigID, &l.TrunkID, &l.TrunkHost, &l.RecordingMode)

	if errors.Is(err, pgx.ErrNoRows) {
		return calls.OutboundLine{}, ErrNotFound
	}
	if err != nil {
		return calls.OutboundLine{}, fmt.Errorf("store: resolving outbound line for %s: %w", number, err)
	}
	return l, nil
}

// ---------------------------------------------------------------------------
// GET /v1/trunks/{id}/coverage
// ---------------------------------------------------------------------------

// Coverage is how a trunk's numbers are divided among lines.
//
// The uncovered sets are the useful part. An inbound gap is numbers that ring
// and are refused as unconfigured; an outbound gap is numbers nobody can dial
// out from. Neither shows up anywhere else until a call fails.
type Coverage struct {
	TrunkID           int64            `json:"trunk_id"`
	DIDRanges         []numrange.Range `json:"did_ranges"`
	Inbound           []LineCoverage   `json:"inbound"`
	Outbound          []LineCoverage   `json:"outbound"`
	UncoveredInbound  []numrange.Range `json:"uncovered_inbound"`
	UncoveredOutbound []numrange.Range `json:"uncovered_outbound"`
}

// LineCoverage is one line's share of the trunk.
type LineCoverage struct {
	TrafficConfigID int64            `json:"traffic_config_id"`
	TenantID        string           `json:"tenant_id"`
	Name            string           `json:"name"`
	Status          string           `json:"status"`
	DIDRanges       []numrange.Range `json:"did_ranges"`
}

// TrunkCoverage reports a trunk's coverage. The caller has already checked
// the trunk is visible to them.
func (p *Pool) TrunkCoverage(ctx context.Context, trunkID int64) (Coverage, error) {
	cov := Coverage{
		TrunkID:  trunkID,
		Inbound:  []LineCoverage{},
		Outbound: []LineCoverage{},
	}

	var all, gapInLo, gapInHi, gapOutLo, gapOutHi, allHi []int64
	err := p.QueryRow(ctx, `
		WITH t AS (
		    SELECT did_ranges FROM trunks WHERE id = $1 AND deleted_at IS NULL
		), used AS (
		    SELECT
		      COALESCE(range_agg(r) FILTER (WHERE c.direction = 'inbound'),  '{}'::int8multirange) AS inbound,
		      COALESCE(range_agg(r) FILTER (WHERE c.direction = 'outbound'), '{}'::int8multirange) AS outbound
		      FROM traffic_configs c
		     CROSS JOIN LATERAL unnest(c.did_ranges) AS r
		     WHERE c.trunk_id = $1 AND c.deleted_at IS NULL
		), gaps AS (
		    SELECT t.did_ranges AS whole,
		           t.did_ranges - used.inbound  AS gap_in,
		           t.did_ranges - used.outbound AS gap_out
		      FROM t CROSS JOIN used
		)
		SELECT `+rangeArraysSQL("whole")+`,
		       `+rangeArraysSQL("gap_in")+`,
		       `+rangeArraysSQL("gap_out")+`
		  FROM gaps`, trunkID,
	).Scan(&all, &allHi, &gapInLo, &gapInHi, &gapOutLo, &gapOutHi)
	if errors.Is(err, pgx.ErrNoRows) {
		return Coverage{}, ErrNotFound
	}
	if err != nil {
		return Coverage{}, fmt.Errorf("store: coverage for trunk %d: %w", trunkID, err)
	}

	if cov.DIDRanges, err = rangesFrom(all, allHi); err != nil {
		return Coverage{}, err
	}
	if cov.UncoveredInbound, err = rangesFrom(gapInLo, gapInHi); err != nil {
		return Coverage{}, err
	}
	if cov.UncoveredOutbound, err = rangesFrom(gapOutLo, gapOutHi); err != nil {
		return Coverage{}, err
	}

	lines, err := p.ListTrafficConfigs(ctx, "", trunkID)
	if err != nil {
		return Coverage{}, err
	}
	for _, c := range lines {
		lc := LineCoverage{
			TrafficConfigID: c.ID, TenantID: c.TenantID, Name: c.Name,
			Status: c.Status, DIDRanges: c.DIDRanges,
		}
		if c.Direction == "inbound" {
			cov.Inbound = append(cov.Inbound, lc)
		} else {
			cov.Outbound = append(cov.Outbound, lc)
		}
	}

	return cov, nil
}

// nullJSON passes an absent document as SQL NULL, so COALESCE can keep the
// column's current value or default. An explicit JSON null counts as absent:
// stored, it would be a jsonb null rather than SQL NULL, and the column would
// hold a value nobody can read keys from.
func nullJSON(m json.RawMessage) any {
	t := strings.TrimSpace(string(m))
	if t == "" || t == "null" {
		return nil
	}
	return t
}

// boundsArg passes a bound array as SQL NULL when the update leaves the ranges
// alone, and as an array — possibly empty — when it sets them.
func boundsArg(set *[]numrange.Bounds, v []int64) any {
	if set == nil {
		return nil
	}
	if v == nil {
		return []int64{}
	}
	return v
}
