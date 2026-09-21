package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/route"
)

// Trunk is a carrier or client SBC.
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

	Type      string `json:"type"`
	Carrier   string `json:"carrier"`
	State     string `json:"state"`
	Status    string `json:"status"`
	IsDefault bool   `json:"is_default"`

	SIPHost       string `json:"sip_host,omitempty"`
	SIPPort       int    `json:"sip_port"`
	SIPUsername   string `json:"sip_username,omitempty"`
	SIPAuthMethod string `json:"sip_auth_method"`

	// HasPassword reports whether a credential is stored, without revealing it.
	// "Is this trunk configured?" is a real question; the answer is a boolean.
	HasPassword bool `json:"has_password"`

	SignallingCIDRs []string `json:"signalling_cidrs"`
	MediaCIDRs      []string `json:"media_cidrs"`
	ChannelSize     *int     `json:"channel_size,omitempty"`
	CallerIDFormat  string   `json:"caller_id_format"`

	// TwilioTrunkSID links this row to a trunk in Twilio, for drift detection.
	// Kept in metadata rather than as a column: it is carrier-specific, and a
	// column per carrier is how a schema acquires twilio_sid, vonage_id,
	// bandwidth_ref and so on.
	TwilioTrunkSID string `json:"twilio_trunk_sid,omitempty"`

	HealthStatus    string     `json:"health_status"`
	HealthCheckedAt *time.Time `json:"health_checked_at,omitempty"`
	HealthSince     *time.Time `json:"health_since,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const trunkColumns = `
	id, tenant_id, slug, name, type, carrier, state, status, is_default,
	COALESCE(sip_host, ''), sip_port, COALESCE(sip_username, ''), sip_auth_method,
	sip_password IS NOT NULL,
	signalling_cidrs, media_cidrs, channel_size, caller_id_format,
	COALESCE(metadata->>'twilio_trunk_sid', ''),
	health_status, health_checked_at, health_since,
	created_at, updated_at`

func scanTrunk(row pgx.Row) (Trunk, error) {
	var t Trunk
	var tenant *string

	err := row.Scan(&t.ID, &tenant, &t.Slug, &t.Name, &t.Type, &t.Carrier,
		&t.State, &t.Status, &t.IsDefault,
		&t.SIPHost, &t.SIPPort, &t.SIPUsername, &t.SIPAuthMethod, &t.HasPassword,
		&t.SignallingCIDRs, &t.MediaCIDRs, &t.ChannelSize, &t.CallerIDFormat,
		&t.TwilioTrunkSID,
		&t.HealthStatus, &t.HealthCheckedAt, &t.HealthSince,
		&t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Trunk{}, err
	}

	t.TenantID = deref(tenant)
	return t, nil
}

func (p *Pool) CreateTrunk(ctx context.Context, t Trunk) (Trunk, error) {
	row := p.QueryRow(ctx, `
		INSERT INTO trunks (
			tenant_id, slug, name, type, carrier, status, is_default,
			sip_host, sip_port, sip_username, sip_auth_method,
			signalling_cidrs, media_cidrs, channel_size, caller_id_format,
			metadata
		) VALUES (
			$1, $2, $3, $4, $5, COALESCE(NULLIF($6, ''), 'inactive'), $7,
			$8, COALESCE(NULLIF($9, 0), 5060), $10, $11,
			$12, $13, $14, COALESCE(NULLIF($15, ''), 'e164'),
			CASE WHEN $16 = '' THEN '{}'::jsonb
			     ELSE jsonb_build_object('twilio_trunk_sid', $16) END
		)
		RETURNING `+trunkColumns,
		nullStringArg(t.TenantID), t.Slug, t.Name, t.Type, t.Carrier,
		t.Status, t.IsDefault,
		nullStringArg(t.SIPHost), t.SIPPort, nullStringArg(t.SIPUsername), t.SIPAuthMethod,
		emptyIfNil(t.SignallingCIDRs), emptyIfNil(t.MediaCIDRs),
		t.ChannelSize, t.CallerIDFormat, t.TwilioTrunkSID)

	out, err := scanTrunk(row)
	if err != nil {
		return Trunk{}, fmt.Errorf("store: creating trunk: %w", err)
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
		 ORDER BY is_default DESC, slug`, nullStringArg(tenantID))
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

// DIDNumbersByTrunk returns the numbers this platform routes over a trunk.
func (p *Pool) DIDNumbersByTrunk(ctx context.Context, trunkID int64) ([]string, error) {
	rows, err := p.Query(ctx,
		`SELECT number FROM dids WHERE trunk_id = $1 AND deleted_at IS NULL ORDER BY number`,
		trunkID)
	if err != nil {
		return nil, fmt.Errorf("store: listing dids for trunk %d: %w", trunkID, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (p *Pool) UpdateTrunk(ctx context.Context, id int64, tenantID string, u TrunkUpdate) (Trunk, error) {
	row := p.QueryRow(ctx, `
		UPDATE trunks
		   SET name             = COALESCE($3, name),
		       status           = COALESCE($4, status),
		       sip_host         = COALESCE($5, sip_host),
		       sip_port         = COALESCE($6, sip_port),
		       signalling_cidrs = COALESCE($7, signalling_cidrs),
		       media_cidrs      = COALESCE($8, media_cidrs),
		       channel_size     = CASE WHEN $10 THEN NULL ELSE COALESCE($9, channel_size) END,
		       metadata         = CASE WHEN $11::text IS NULL THEN metadata
		                               ELSE metadata || jsonb_build_object('twilio_trunk_sid', $11) END,
		       updated_at       = now()
		 WHERE id = $1 AND deleted_at IS NULL
		   AND ($2::text IS NULL OR tenant_id = $2)
		RETURNING `+trunkColumns,
		id, nullStringArg(tenantID), u.Name, u.Status, u.SIPHost, u.SIPPort,
		u.SignallingCIDRs, u.MediaCIDRs, u.ChannelSize, u.ClearChannelSize,
		u.TwilioTrunkSID)

	t, err := scanTrunk(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Trunk{}, ErrNotFound
	}
	if err != nil {
		return Trunk{}, fmt.Errorf("store: updating trunk: %w", err)
	}
	return t, nil
}

// TrunkUpdate is the patchable subset.
//
// type, carrier and sip_auth_method are NOT here. Changing what kind of trunk
// a row is, in place, while DIDs point at it, is a migration rather than an
// edit — and doing it by accident reroutes live traffic.
type TrunkUpdate struct {
	Name             *string
	Status           *string
	SIPHost          *string
	SIPPort          *int
	SignallingCIDRs  []string
	MediaCIDRs       []string
	ChannelSize      *int
	ClearChannelSize bool
	TwilioTrunkSID   *string
}

func (p *Pool) DeleteTrunk(ctx context.Context, id int64, tenantID string) error {
	// Refuse while DIDs still point at it. A soft-deleted trunk leaves those
	// DIDs resolving to a trunk with no host, and the symptom is an outbound
	// leg with a Request-URI pointing at nothing.
	var inUse int
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM dids WHERE trunk_id = $1 AND deleted_at IS NULL`,
		id).Scan(&inUse); err != nil {
		return fmt.Errorf("store: checking trunk use: %w", err)
	}
	if inUse > 0 {
		return fmt.Errorf("%w: %d DIDs still route by this trunk", ErrInUse, inUse)
	}

	tag, err := p.Exec(ctx, `
		UPDATE trunks SET deleted_at = now(), updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL AND ($2::text IS NULL OR tenant_id = $2)`,
		id, nullStringArg(tenantID))
	if err != nil {
		return fmt.Errorf("store: deleting trunk: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ErrInUse means a row cannot be removed because something still references it.
var ErrInUse = errors.New("store: still in use")

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
type HandoffTarget struct {
	ID          int64     `json:"id"`
	TenantID    string    `json:"tenant_id"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	Destination string    `json:"destination"`
	Priority    int       `json:"priority"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (p *Pool) CreateHandoffTarget(ctx context.Context, h HandoffTarget) (HandoffTarget, error) {
	err := p.QueryRow(ctx, `
		INSERT INTO handoff_targets (tenant_id, name, kind, destination, priority, enabled)
		VALUES ($1, $2, $3, $4, COALESCE(NULLIF($5, 0), 100), $6)
		RETURNING id, tenant_id, name, kind, destination, priority, enabled, created_at, updated_at`,
		h.TenantID, h.Name, h.Kind, h.Destination, h.Priority, h.Enabled,
	).Scan(&h.ID, &h.TenantID, &h.Name, &h.Kind, &h.Destination,
		&h.Priority, &h.Enabled, &h.CreatedAt, &h.UpdatedAt)
	if err != nil {
		return HandoffTarget{}, fmt.Errorf("store: creating handoff target: %w", err)
	}
	return h, nil
}

// ListHandoffTargets returns targets for the API, including disabled ones.
//
// Different from route.HandoffTargets, which returns only what a live call may
// use. An operator needs to see the disabled one to enable it; a call must
// never be routed to it.
func (p *Pool) ListHandoffTargets(ctx context.Context, tenantID string) ([]HandoffTarget, error) {
	rows, err := p.Query(ctx, `
		SELECT id, tenant_id, name, kind, destination, priority, enabled, created_at, updated_at
		  FROM handoff_targets
		 WHERE deleted_at IS NULL AND ($1::text IS NULL OR tenant_id = $1)
		 ORDER BY tenant_id, priority, id`, nullStringArg(tenantID))
	if err != nil {
		return nil, fmt.Errorf("store: listing handoff targets: %w", err)
	}
	defer rows.Close()

	out := []HandoffTarget{}
	for rows.Next() {
		var h HandoffTarget
		if err := rows.Scan(&h.ID, &h.TenantID, &h.Name, &h.Kind, &h.Destination,
			&h.Priority, &h.Enabled, &h.CreatedAt, &h.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (p *Pool) UpdateHandoffTarget(ctx context.Context, id int64, tenantID string, name, kind, destination *string, priority *int, enabled *bool) (HandoffTarget, error) {
	var h HandoffTarget
	err := p.QueryRow(ctx, `
		UPDATE handoff_targets
		   SET name        = COALESCE($3, name),
		       kind        = COALESCE($4, kind),
		       destination = COALESCE($5, destination),
		       priority    = COALESCE($6, priority),
		       enabled     = COALESCE($7, enabled),
		       updated_at  = now()
		 WHERE id = $1 AND deleted_at IS NULL AND ($2::text IS NULL OR tenant_id = $2)
		RETURNING id, tenant_id, name, kind, destination, priority, enabled, created_at, updated_at`,
		id, nullStringArg(tenantID), name, kind, destination, priority, enabled,
	).Scan(&h.ID, &h.TenantID, &h.Name, &h.Kind, &h.Destination,
		&h.Priority, &h.Enabled, &h.CreatedAt, &h.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return HandoffTarget{}, ErrNotFound
	}
	if err != nil {
		return HandoffTarget{}, fmt.Errorf("store: updating handoff target: %w", err)
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

// ---------------------------------------------------------------------------
// Agent config — written with the plan, not on its own
// ---------------------------------------------------------------------------

// UpsertAgentConfig writes the AI settings for a plan.
//
// Upsert rather than create/update, because there is exactly one per plan and
// the caller does not care which it is doing — that is an artefact of storage,
// not something an operator should have to know.
func (p *Pool) UpsertAgentConfig(ctx context.Context, tenantID, planID string, c route.AgentConfig) error {
	_, err := p.Exec(ctx, `
		INSERT INTO agent_configs (tenant_id, plan_id, room_template, agent_identity)
		VALUES ($1, $2, COALESCE(NULLIF($3, ''), '{call_uuid}'), $4)
		ON CONFLICT (plan_id) WHERE deleted_at IS NULL
		DO UPDATE SET room_template  = EXCLUDED.room_template,
		              agent_identity = EXCLUDED.agent_identity,
		              updated_at     = now()`,
		tenantID, planID, c.RoomTemplate, c.AgentIdentity)
	if err != nil {
		return fmt.Errorf("store: upserting agent config for plan %s: %w", planID, err)
	}
	return nil
}
