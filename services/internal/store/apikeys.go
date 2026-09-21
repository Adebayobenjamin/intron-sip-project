package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/controlplane/auth"
)

// PrincipalByTokenHash resolves an API key.
//
// Revocation and expiry are checked in SQL rather than in Go, so a revoked key
// cannot be used by a code path that forgot to look. `revoked_at IS NULL` and
// the expiry check are the whole security boundary for this table.
func (p *Pool) PrincipalByTokenHash(ctx context.Context, hash []byte) (auth.Principal, error) {
	var (
		tenantID *string
		label    string
		scopes   []string
		revoked  bool
	)

	err := p.QueryRow(ctx, `
		SELECT tenant_id,
		       label,
		       scopes,
		       (revoked_at IS NOT NULL
		         OR (expires_at IS NOT NULL AND expires_at <= now())) AS revoked
		  FROM api_keys
		 WHERE token_hash = $1`, hash).Scan(&tenantID, &label, &scopes, &revoked)

	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Principal{}, auth.ErrUnknownToken
	}
	if err != nil {
		return auth.Principal{}, fmt.Errorf("store: looking up api key: %w", err)
	}

	if revoked {
		// Distinct from "unknown" on purpose: a key that used to work and
		// stopped is a different conversation from one that never existed,
		// and the holder can tell the two apart anyway.
		return auth.Principal{}, auth.ErrExpired
	}

	// Record use, best effort. It is the only way to answer "is this key still
	// in use?" before revoking one, and failing the request because the
	// bookkeeping write failed would be absurd.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = p.Exec(ctx,
			`UPDATE api_keys SET last_used_at = now() WHERE token_hash = $1`, hash)
	}()

	pr := auth.Principal{Label: label, Scopes: scopes}
	if tenantID != nil {
		pr.TenantID = *tenantID
	}
	return pr, nil
}

// APIKey is a key as returned by the API. It never carries the token.
type APIKey struct {
	ID         int64      `json:"id"`
	TenantID   string     `json:"tenant_id,omitempty"`
	Label      string     `json:"label"`
	Scopes     []string   `json:"scopes"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// CreateAPIKey stores a new key and returns it. The caller holds the plaintext
// token; this never sees it in a form it could store.
func (p *Pool) CreateAPIKey(ctx context.Context, tenantID, label string, scopes []string, hash []byte, expiresAt *time.Time) (APIKey, error) {
	var k APIKey
	var tid *string

	err := p.QueryRow(ctx, `
		INSERT INTO api_keys (tenant_id, label, token_hash, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, tenant_id, label, scopes, expires_at, created_at`,
		nullStringArg(tenantID), label, hash, scopes, expiresAt,
	).Scan(&k.ID, &tid, &k.Label, &k.Scopes, &k.ExpiresAt, &k.CreatedAt)
	if err != nil {
		return APIKey{}, fmt.Errorf("store: creating api key: %w", err)
	}

	if tid != nil {
		k.TenantID = *tid
	}
	return k, nil
}

// ListAPIKeys returns keys, scoped to a tenant when one is given.
func (p *Pool) ListAPIKeys(ctx context.Context, tenantID string) ([]APIKey, error) {
	rows, err := p.Query(ctx, `
		SELECT id, tenant_id, label, scopes, last_used_at, expires_at, created_at, revoked_at
		  FROM api_keys
		 WHERE ($1::text IS NULL OR tenant_id = $1)
		 ORDER BY created_at DESC`, nullStringArg(tenantID))
	if err != nil {
		return nil, fmt.Errorf("store: listing api keys: %w", err)
	}
	defer rows.Close()

	out := []APIKey{}
	for rows.Next() {
		var k APIKey
		var tid *string
		if err := rows.Scan(&k.ID, &tid, &k.Label, &k.Scopes,
			&k.LastUsedAt, &k.ExpiresAt, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, fmt.Errorf("store: scanning api key: %w", err)
		}
		if tid != nil {
			k.TenantID = *tid
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey marks a key unusable.
//
// A soft revoke rather than a delete: "which key was that, and when did it
// stop working" is the question asked after an incident, and a deleted row
// cannot answer it.
func (p *Pool) RevokeAPIKey(ctx context.Context, id int64, tenantID string) error {
	tag, err := p.Exec(ctx, `
		UPDATE api_keys
		   SET revoked_at = now()
		 WHERE id = $1
		   AND ($2::text IS NULL OR tenant_id = $2)
		   AND revoked_at IS NULL`, id, nullStringArg(tenantID))
	if err != nil {
		return fmt.Errorf("store: revoking api key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
