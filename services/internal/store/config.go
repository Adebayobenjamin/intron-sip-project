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
		return Tenant{}, fmt.Errorf("store: creating tenant: %w", classify(err))
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
