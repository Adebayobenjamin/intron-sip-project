package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// A note on the list queries below: each initialises its slice rather than
// declaring a nil one. A nil Go slice marshals to JSON `null`, and an API that
// returns null for "no rows" breaks every client that does data.map(...) —
// which is every client. Empty means [].

// ErrNotFound means no row matched — including rows that exist but belong to
// another tenant.
//
// The two are deliberately indistinguishable. Answering "forbidden" for a row
// that exists but is not yours confirms it exists, which on a multi-tenant
// platform lets anyone enumerate other tenants' identifiers.
var ErrNotFound = errors.New("store: not found")

// ---------------------------------------------------------------------------
// Tenants
// ---------------------------------------------------------------------------

type Tenant struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Status         string     `json:"status"`
	ConcurrencyCap *int       `json:"concurrency_cap,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeletedAt      *time.Time `json:"-"`
}

func (p *Pool) CreateTenant(ctx context.Context, t Tenant) (Tenant, error) {
	err := p.QueryRow(ctx, `
		INSERT INTO tenants (id, name, status, concurrency_cap)
		VALUES ($1, $2, COALESCE(NULLIF($3, ''), 'active'), $4)
		RETURNING id, name, status, concurrency_cap, created_at, updated_at`,
		t.ID, t.Name, t.Status, t.ConcurrencyCap,
	).Scan(&t.ID, &t.Name, &t.Status, &t.ConcurrencyCap, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Tenant{}, fmt.Errorf("store: creating tenant: %w", err)
	}
	return t, nil
}

func (p *Pool) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := p.Query(ctx, `
		SELECT id, name, status, concurrency_cap, created_at, updated_at
		  FROM tenants WHERE deleted_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: listing tenants: %w", err)
	}
	defer rows.Close()

	out := []Tenant{}
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.Name, &t.Status, &t.ConcurrencyCap,
			&t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (p *Pool) GetTenant(ctx context.Context, id string) (Tenant, error) {
	var t Tenant
	err := p.QueryRow(ctx, `
		SELECT id, name, status, concurrency_cap, created_at, updated_at
		  FROM tenants WHERE id = $1 AND deleted_at IS NULL`, id,
	).Scan(&t.ID, &t.Name, &t.Status, &t.ConcurrencyCap, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("store: getting tenant: %w", err)
	}
	return t, nil
}

// UpdateTenant applies only the fields that were sent. COALESCE on a nullable
// argument is what makes a PATCH partial — passing NULL leaves the column as
// it was, rather than blanking a name because the caller only meant to change
// the status.
func (p *Pool) UpdateTenant(ctx context.Context, id string, name, status *string, cap *int, clearCap bool) (Tenant, error) {
	var t Tenant
	err := p.QueryRow(ctx, `
		UPDATE tenants
		   SET name            = COALESCE($2, name),
		       status          = COALESCE($3, status),
		       concurrency_cap = CASE WHEN $5 THEN NULL ELSE COALESCE($4, concurrency_cap) END,
		       updated_at      = now()
		 WHERE id = $1 AND deleted_at IS NULL
		RETURNING id, name, status, concurrency_cap, created_at, updated_at`,
		id, name, status, cap, clearCap,
	).Scan(&t.ID, &t.Name, &t.Status, &t.ConcurrencyCap, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("store: updating tenant: %w", err)
	}
	return t, nil
}

// DeleteTenant soft-deletes.
//
// Never a hard delete: call_history rows reference the tenant by id and are
// kept for years, so removing the row would orphan every record of every call
// that tenant ever made.
func (p *Pool) DeleteTenant(ctx context.Context, id string) error {
	tag, err := p.Exec(ctx,
		`UPDATE tenants SET deleted_at = now(), updated_at = now()
		  WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("store: deleting tenant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Inbound plans
// ---------------------------------------------------------------------------

type Plan struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Destination string    `json:"destination"`
	Recording   bool      `json:"recording"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (p *Pool) CreatePlan(ctx context.Context, pl Plan) (Plan, error) {
	err := p.QueryRow(ctx, `
		INSERT INTO inbound_plans (id, tenant_id, name, status, destination, recording)
		VALUES ($1, $2, $3, COALESCE(NULLIF($4, ''), 'active'), $5, $6)
		RETURNING id, tenant_id, name, status, destination, recording, created_at, updated_at`,
		pl.ID, pl.TenantID, pl.Name, pl.Status, pl.Destination, pl.Recording,
	).Scan(&pl.ID, &pl.TenantID, &pl.Name, &pl.Status, &pl.Destination,
		&pl.Recording, &pl.CreatedAt, &pl.UpdatedAt)
	if err != nil {
		return Plan{}, fmt.Errorf("store: creating plan: %w", err)
	}
	return pl, nil
}

func (p *Pool) ListPlans(ctx context.Context, tenantID string) ([]Plan, error) {
	rows, err := p.Query(ctx, `
		SELECT id, tenant_id, name, status, destination, recording, created_at, updated_at
		  FROM inbound_plans
		 WHERE deleted_at IS NULL AND ($1::text IS NULL OR tenant_id = $1)
		 ORDER BY tenant_id, id`, nullStringArg(tenantID))
	if err != nil {
		return nil, fmt.Errorf("store: listing plans: %w", err)
	}
	defer rows.Close()

	out := []Plan{}
	for rows.Next() {
		var pl Plan
		if err := rows.Scan(&pl.ID, &pl.TenantID, &pl.Name, &pl.Status,
			&pl.Destination, &pl.Recording, &pl.CreatedAt, &pl.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, pl)
	}
	return out, rows.Err()
}

func (p *Pool) UpdatePlan(ctx context.Context, id, tenantID string, name, status, destination *string, recording *bool) (Plan, error) {
	var pl Plan
	err := p.QueryRow(ctx, `
		UPDATE inbound_plans
		   SET name        = COALESCE($3, name),
		       status      = COALESCE($4, status),
		       destination = COALESCE($5, destination),
		       recording   = COALESCE($6, recording),
		       updated_at  = now()
		 WHERE id = $1 AND deleted_at IS NULL
		   AND ($2::text IS NULL OR tenant_id = $2)
		RETURNING id, tenant_id, name, status, destination, recording, created_at, updated_at`,
		id, nullStringArg(tenantID), name, status, destination, recording,
	).Scan(&pl.ID, &pl.TenantID, &pl.Name, &pl.Status, &pl.Destination,
		&pl.Recording, &pl.CreatedAt, &pl.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, ErrNotFound
	}
	if err != nil {
		return Plan{}, fmt.Errorf("store: updating plan: %w", err)
	}
	return pl, nil
}

func (p *Pool) DeletePlan(ctx context.Context, id, tenantID string) error {
	tag, err := p.Exec(ctx, `
		UPDATE inbound_plans SET deleted_at = now(), updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL AND ($2::text IS NULL OR tenant_id = $2)`,
		id, nullStringArg(tenantID))
	if err != nil {
		return fmt.Errorf("store: deleting plan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// DIDs
// ---------------------------------------------------------------------------

type DID struct {
	ID       int64  `json:"id"`
	Number   string `json:"number"`
	TenantID string `json:"tenant_id"`
	PlanID   string `json:"plan_id,omitempty"`
	TrunkID  int64  `json:"trunk_id,omitempty"`
	Status   string `json:"status"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateDID stores a number. The caller normalises it first — see
// admission.Normalise — so the unique index means what it says.
func (p *Pool) CreateDID(ctx context.Context, d DID) (DID, error) {
	var plan *string
	var trunk *int64
	if d.PlanID != "" {
		plan = &d.PlanID
	}
	if d.TrunkID != 0 {
		trunk = &d.TrunkID
	}

	var gotPlan *string
	var gotTrunk *int64

	err := p.QueryRow(ctx, `
		INSERT INTO dids (number, tenant_id, plan_id, trunk_id, status)
		VALUES ($1, $2, $3, $4, COALESCE(NULLIF($5, ''), 'active'))
		RETURNING id, number, tenant_id, plan_id, trunk_id, status, created_at, updated_at`,
		d.Number, d.TenantID, plan, trunk, d.Status,
	).Scan(&d.ID, &d.Number, &d.TenantID, &gotPlan, &gotTrunk, &d.Status,
		&d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return DID{}, fmt.Errorf("store: creating did: %w", err)
	}

	d.PlanID, d.TrunkID = deref(gotPlan), derefInt(gotTrunk)
	return d, nil
}

func (p *Pool) ListDIDs(ctx context.Context, tenantID string) ([]DID, error) {
	rows, err := p.Query(ctx, `
		SELECT id, number, tenant_id, plan_id, trunk_id, status, created_at, updated_at
		  FROM dids
		 WHERE deleted_at IS NULL AND ($1::text IS NULL OR tenant_id = $1)
		 ORDER BY number`, nullStringArg(tenantID))
	if err != nil {
		return nil, fmt.Errorf("store: listing dids: %w", err)
	}
	defer rows.Close()

	out := []DID{}
	for rows.Next() {
		var d DID
		var plan *string
		var trunk *int64
		if err := rows.Scan(&d.ID, &d.Number, &d.TenantID, &plan, &trunk,
			&d.Status, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		d.PlanID, d.TrunkID = deref(plan), derefInt(trunk)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (p *Pool) UpdateDID(ctx context.Context, id int64, tenantID string, planID *string, trunkID *int64, status *string) (DID, error) {
	var d DID
	var plan *string
	var trunk *int64

	err := p.QueryRow(ctx, `
		UPDATE dids
		   SET plan_id    = COALESCE($3, plan_id),
		       trunk_id   = COALESCE($4, trunk_id),
		       status     = COALESCE($5, status),
		       updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL AND ($2::text IS NULL OR tenant_id = $2)
		RETURNING id, number, tenant_id, plan_id, trunk_id, status, created_at, updated_at`,
		id, nullStringArg(tenantID), planID, trunkID, status,
	).Scan(&d.ID, &d.Number, &d.TenantID, &plan, &trunk, &d.Status,
		&d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DID{}, ErrNotFound
	}
	if err != nil {
		return DID{}, fmt.Errorf("store: updating did: %w", err)
	}

	d.PlanID, d.TrunkID = deref(plan), derefInt(trunk)
	return d, nil
}

func (p *Pool) DeleteDID(ctx context.Context, id int64, tenantID string) error {
	tag, err := p.Exec(ctx, `
		UPDATE dids SET deleted_at = now(), updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL AND ($2::text IS NULL OR tenant_id = $2)`,
		id, nullStringArg(tenantID))
	if err != nil {
		return fmt.Errorf("store: deleting did: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
