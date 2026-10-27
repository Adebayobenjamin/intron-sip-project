package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/numrange"
)

// Trunk is a carrier or client SBC, and the numbers it carries.
//
// TenantID empty is the platform trunk, whose numbers are leased to tenants
// line by line; set, it is the client's own (BYOT).
//
// NOTE WHAT IS ABSENT: sip_password. It is encrypted at the application layer
// and never leaves the database through this struct — a carrier credential in
// a JSON response is a toll-fraud incident, and there is no operational reason
// to read one back.
type Trunk struct {
	ID       int64  `json:"id"`
	TenantID string `json:"tenant_id,omitempty"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`

	Carrier string `json:"carrier"`
	State   string `json:"state"`
	Status  string `json:"status"`

	SIPHost       string `json:"sip_host,omitempty"`
	SIPPort       int    `json:"sip_port"`
	SIPUsername   string `json:"sip_username,omitempty"`
	SIPAuthMethod string `json:"sip_auth_method"`

	// HasPassword reports whether a credential is stored, without revealing it.
	// "Is this trunk configured?" is a real question; the answer is a boolean.
	HasPassword bool `json:"has_password"`

	SignallingCIDRs []string `json:"signalling_cidrs"`
	MediaCIDRs      []string `json:"media_cidrs"`

	// DIDRanges is the number inventory: every number a carrier can deliver
	// on this trunk, as inclusive E.164 blocks. Adjacent blocks come back
	// merged.
	DIDRanges []numrange.Range `json:"did_ranges"`

	ChannelSize    *int   `json:"channel_size,omitempty"`
	CallerIDFormat string `json:"caller_id_format"`

	HealthStatus    string     `json:"health_status"`
	HealthCheckedAt *time.Time `json:"health_checked_at,omitempty"`
	HealthSince     *time.Time `json:"health_since,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var trunkColumns = `
	id, tenant_id, slug, name, carrier, state, status,
	COALESCE(sip_host, ''), sip_port, COALESCE(sip_username, ''), sip_auth_method,
	sip_password IS NOT NULL,
	signalling_cidrs, media_cidrs,
	` + rangeArraysSQL("did_ranges") + `,
	channel_size, caller_id_format,
	health_status, health_checked_at, health_since,
	created_at, updated_at`

func scanTrunk(row pgx.Row) (Trunk, error) {
	var t Trunk
	var tenant *string
	var lowers, uppers []int64

	err := row.Scan(&t.ID, &tenant, &t.Slug, &t.Name, &t.Carrier,
		&t.State, &t.Status,
		&t.SIPHost, &t.SIPPort, &t.SIPUsername, &t.SIPAuthMethod, &t.HasPassword,
		&t.SignallingCIDRs, &t.MediaCIDRs, &lowers, &uppers,
		&t.ChannelSize, &t.CallerIDFormat,
		&t.HealthStatus, &t.HealthCheckedAt, &t.HealthSince,
		&t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Trunk{}, err
	}

	if t.DIDRanges, err = rangesFrom(lowers, uppers); err != nil {
		return Trunk{}, err
	}
	t.TenantID = deref(tenant)
	return t, nil
}

// CreateTrunk stores a trunk and its numbers. ranges arrive validated; the
// database refuses any of them already carried by another trunk.
func (p *Pool) CreateTrunk(ctx context.Context, t Trunk, ranges []numrange.Bounds) (Trunk, error) {
	lowers, uppers := numrange.Columns(ranges)

	row := p.QueryRow(ctx, `
		INSERT INTO trunks (
			tenant_id, slug, name, carrier, status,
			sip_host, sip_port, sip_username, sip_auth_method,
			signalling_cidrs, media_cidrs, did_ranges, channel_size, caller_id_format,
			metadata
		) VALUES (
			$1, $2, $3, $4, COALESCE(NULLIF($5, ''), 'inactive'),
			$6, COALESCE(NULLIF($7, 0), 5060), $8, $9,
			$10, $11, `+rangeFromArraysSQL("$12", "$13")+`,
			$14, COALESCE(NULLIF($15, ''), 'e164'),
			'{}'::jsonb
		)
		RETURNING `+trunkColumns,
		nullStringArg(t.TenantID), t.Slug, t.Name, t.Carrier, t.Status,
		nullStringArg(t.SIPHost), t.SIPPort, nullStringArg(t.SIPUsername), t.SIPAuthMethod,
		emptyIfNil(t.SignallingCIDRs), emptyIfNil(t.MediaCIDRs), lowers, uppers,
		t.ChannelSize, t.CallerIDFormat)

	out, err := scanTrunk(row)
	if err != nil {
		return Trunk{}, fmt.Errorf("store: creating trunk: %w", classify(err))
	}
	return out, nil
}

func (p *Pool) ListTrunks(ctx context.Context, tenantID string) ([]Trunk, error) {
	// A trunk with no tenant is platform-wide — the shared carrier every
	// tenant leaves by. A tenant-scoped list must include it, or a tenant
	// appears to have no way out at all.
	rows, err := p.Query(ctx, `
		SELECT `+trunkColumns+`
		  FROM trunks
		 WHERE deleted_at IS NULL
		   AND ($1::text IS NULL OR tenant_id = $1 OR tenant_id IS NULL)
		 ORDER BY tenant_id NULLS FIRST, slug`, nullStringArg(tenantID))
	if err != nil {
		return nil, fmt.Errorf("store: listing trunks: %w", err)
	}
	defer rows.Close()

	out := []Trunk{}
	for rows.Next() {
		t, err := scanTrunk(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scanning trunk: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTrunk reads one trunk.
func (p *Pool) GetTrunk(ctx context.Context, id int64, tenantID string) (Trunk, error) {
	row := p.QueryRow(ctx, `
		SELECT `+trunkColumns+`
		  FROM trunks
		 WHERE id = $1 AND deleted_at IS NULL
		   AND ($2::text IS NULL OR tenant_id = $2 OR tenant_id IS NULL)`,
		id, nullStringArg(tenantID))

	t, err := scanTrunk(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Trunk{}, ErrNotFound
	}
	if err != nil {
		return Trunk{}, fmt.Errorf("store: getting trunk: %w", err)
	}
	return t, nil
}

func (p *Pool) UpdateTrunk(ctx context.Context, id int64, tenantID string, u TrunkUpdate) (Trunk, error) {
	var lowers, uppers []int64
	if u.DIDRanges != nil {
		lowers, uppers = numrange.Columns(*u.DIDRanges)
	}

	row := p.QueryRow(ctx, `
		UPDATE trunks
		   SET name             = COALESCE($3, name),
		       status           = COALESCE($4, status),
		       sip_host         = COALESCE($5, sip_host),
		       sip_port         = COALESCE($6, sip_port),
		       signalling_cidrs = COALESCE($7, signalling_cidrs),
		       media_cidrs      = COALESCE($8, media_cidrs),
		       channel_size     = CASE WHEN $10 THEN NULL ELSE COALESCE($9, channel_size) END,
		       did_ranges       = CASE WHEN $11::int8[] IS NULL THEN did_ranges
		                               ELSE `+rangeFromArraysSQL("$11", "$12")+` END,
		       updated_at       = now()
		 WHERE id = $1 AND deleted_at IS NULL
		   AND ($2::text IS NULL OR tenant_id = $2)
		RETURNING `+trunkColumns,
		id, nullStringArg(tenantID), u.Name, u.Status, u.SIPHost, u.SIPPort,
		u.SignallingCIDRs, u.MediaCIDRs, u.ChannelSize, u.ClearChannelSize,
		boundsArg(u.DIDRanges, lowers), boundsArg(u.DIDRanges, uppers))

	t, err := scanTrunk(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Trunk{}, ErrNotFound
	}
	if err != nil {
		return Trunk{}, fmt.Errorf("store: updating trunk: %w", classify(err))
	}
	return t, nil
}

// TrunkUpdate is the patchable subset.
//
// carrier and sip_auth_method are NOT here. Changing what kind of trunk a row
// is, in place, while lines sit on it, is a migration rather than an edit —
// and doing it by accident reroutes live traffic.
type TrunkUpdate struct {
	Name             *string
	Status           *string
	SIPHost          *string
	SIPPort          *int
	SignallingCIDRs  []string
	MediaCIDRs       []string
	ChannelSize      *int
	ClearChannelSize bool

	// DIDRanges nil leaves the inventory alone; set, it REPLACES it. The
	// database refuses a replacement that drops numbers a line still uses.
	DIDRanges *[]numrange.Bounds
}

// DeleteTrunk soft-deletes a trunk.
//
// Refused while lines still sit on it — by the database, not here. A trunk
// deleted under a line leaves the line admitting calls no carrier delivers,
// and an E.164 handoff dialling out over a trunk with no host.
func (p *Pool) DeleteTrunk(ctx context.Context, id int64, tenantID string) error {
	tag, err := p.Exec(ctx, `
		UPDATE trunks SET deleted_at = now(), updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL AND ($2::text IS NULL OR tenant_id = $2)`,
		id, nullStringArg(tenantID))
	if err != nil {
		return fmt.Errorf("store: deleting trunk: %w", classify(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// emptyIfNil turns a nil slice into an empty one.
//
// A nil Go slice is encoded as SQL NULL, and these columns are NOT NULL with a
// '{}' default — so omitting the field from a request would fail the insert
// rather than taking the default. On UPDATE the nil is wanted, because there
// COALESCE reads it as "leave this column alone".
func emptyIfNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// ---------------------------------------------------------------------------
// Handoff targets
// ---------------------------------------------------------------------------

// HandoffTarget is where a call goes when it needs a human.
//
// TrafficConfigID empty puts the target in the tenant's DEFAULT list; set, it
// is an override for that one line. A line with any enabled override uses only
// its overrides.
type HandoffTarget struct {
	ID              int64     `json:"id"`
	TenantID        string    `json:"tenant_id"`
	TrafficConfigID *int64    `json:"traffic_config_id,omitempty"`
	Name            string    `json:"name"`
	Kind            string    `json:"kind"`
	Destination     string    `json:"destination"`
	Priority        int       `json:"priority"`
	Enabled         bool      `json:"enabled"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

const handoffTargetColumns = `
	id, tenant_id, traffic_config_id, name, kind, destination, priority, enabled,
	created_at, updated_at`

func scanHandoffTarget(row pgx.Row) (HandoffTarget, error) {
	var h HandoffTarget
	err := row.Scan(&h.ID, &h.TenantID, &h.TrafficConfigID, &h.Name, &h.Kind,
		&h.Destination, &h.Priority, &h.Enabled, &h.CreatedAt, &h.UpdatedAt)
	return h, err
}

// CreateHandoffTarget stores a target. An override naming another tenant's
// line is refused by the database.
func (p *Pool) CreateHandoffTarget(ctx context.Context, h HandoffTarget) (HandoffTarget, error) {
	out, err := scanHandoffTarget(p.QueryRow(ctx, `
		INSERT INTO handoff_targets (tenant_id, traffic_config_id, name, kind, destination, priority, enabled)
		VALUES ($1, $2, $3, $4, $5, COALESCE(NULLIF($6, 0), 100), $7)
		RETURNING `+handoffTargetColumns,
		h.TenantID, h.TrafficConfigID, h.Name, h.Kind, h.Destination, h.Priority, h.Enabled))
	if err != nil {
		return HandoffTarget{}, fmt.Errorf("store: creating handoff target: %w", classify(err))
	}
	return out, nil
}

// ListHandoffTargets returns targets for the API, including disabled ones.
//
// Different from HandoffTargets in routing.go, which returns only what a live
// call may use. An operator needs to see the disabled one to enable it; a call
// must never be routed to it.
func (p *Pool) ListHandoffTargets(ctx context.Context, tenantID string) ([]HandoffTarget, error) {
	rows, err := p.Query(ctx, `
		SELECT `+handoffTargetColumns+`
		  FROM handoff_targets
		 WHERE deleted_at IS NULL AND ($1::text IS NULL OR tenant_id = $1)
		 ORDER BY tenant_id, traffic_config_id NULLS FIRST, priority, id`, nullStringArg(tenantID))
	if err != nil {
		return nil, fmt.Errorf("store: listing handoff targets: %w", err)
	}
	defer rows.Close()

	out := []HandoffTarget{}
	for rows.Next() {
		h, err := scanHandoffTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// HandoffTargetUpdate is the patchable subset.
type HandoffTargetUpdate struct {
	Name        *string
	Kind        *string
	Destination *string
	Priority    *int
	Enabled     *bool

	// TrafficConfigID moves the target onto a line's override list;
	// ClearTrafficConfig moves it back to the tenant default.
	TrafficConfigID    *int64
	ClearTrafficConfig bool
}

func (p *Pool) UpdateHandoffTarget(ctx context.Context, id int64, tenantID string, u HandoffTargetUpdate) (HandoffTarget, error) {
	h, err := scanHandoffTarget(p.QueryRow(ctx, `
		UPDATE handoff_targets
		   SET name              = COALESCE($3, name),
		       kind              = COALESCE($4, kind),
		       destination       = COALESCE($5, destination),
		       priority          = COALESCE($6, priority),
		       enabled           = COALESCE($7, enabled),
		       traffic_config_id = CASE WHEN $9 THEN NULL
		                                ELSE COALESCE($8, traffic_config_id) END,
		       updated_at        = now()
		 WHERE id = $1 AND deleted_at IS NULL AND ($2::text IS NULL OR tenant_id = $2)
		RETURNING `+handoffTargetColumns,
		id, nullStringArg(tenantID), u.Name, u.Kind, u.Destination, u.Priority, u.Enabled,
		u.TrafficConfigID, u.ClearTrafficConfig))
	if errors.Is(err, pgx.ErrNoRows) {
		return HandoffTarget{}, ErrNotFound
	}
	if err != nil {
		return HandoffTarget{}, fmt.Errorf("store: updating handoff target: %w", classify(err))
	}
	return h, nil
}

func (p *Pool) DeleteHandoffTarget(ctx context.Context, id int64, tenantID string) error {
	tag, err := p.Exec(ctx, `
		UPDATE handoff_targets SET deleted_at = now(), updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL AND ($2::text IS NULL OR tenant_id = $2)`,
		id, nullStringArg(tenantID))
	if err != nil {
		return fmt.Errorf("store: deleting handoff target: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
