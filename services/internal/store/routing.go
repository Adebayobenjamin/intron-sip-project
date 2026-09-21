package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/route"
)

// HandoffTargets returns where a tenant's calls go when they need a human, in
// the order they should be tried.
//
// Ordered by priority then id. The id tiebreak is not decoration: two targets
// at the same priority would otherwise come back in whatever order the planner
// chose, so the same call could take a different route on two identical
// attempts and nobody could reproduce a report about it.
func (p *Pool) HandoffTargets(ctx context.Context, tenantID string) ([]route.HandoffTarget, error) {
	rows, err := p.Query(ctx, `
		SELECT h.id, h.kind, h.destination, COALESCE(t.sip_host, '')
		  FROM handoff_targets h
		  LEFT JOIN trunks t
		    ON t.tenant_id = h.tenant_id
		   AND t.is_default
		   AND t.deleted_at IS NULL
		 WHERE h.tenant_id = $1
		   AND h.enabled
		   AND h.deleted_at IS NULL
		 ORDER BY h.priority, h.id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: loading handoff targets for %s: %w", tenantID, err)
	}
	defer rows.Close()

	var out []route.HandoffTarget
	for rows.Next() {
		var t route.HandoffTarget
		if err := rows.Scan(&t.ID, &t.Kind, &t.Destination, &t.TrunkHost); err != nil {
			return nil, fmt.Errorf("store: scanning handoff target: %w", err)
		}
		out = append(out, t)
	}

	return out, rows.Err()
}

// AgentConfig returns what the AI leg is handed for a plan.
//
// A plan with no row is not an error. It means nobody has configured the agent
// yet, and the caller decides whether that is fatal — for a plan that routes to
// `ai` it is, and for one that routes to `handoff` the row is irrelevant.
func (p *Pool) AgentConfig(ctx context.Context, planID string) (route.AgentConfig, error) {
	var c route.AgentConfig

	err := p.QueryRow(ctx, `
		SELECT room_template, agent_identity
		  FROM agent_configs
		 WHERE plan_id = $1
		   AND deleted_at IS NULL
		 LIMIT 1`, planID).Scan(&c.RoomTemplate, &c.AgentIdentity)

	if errors.Is(err, pgx.ErrNoRows) {
		return route.AgentConfig{}, route.ErrNoAgentConfig
	}
	if err != nil {
		return route.AgentConfig{}, fmt.Errorf("store: loading agent config for plan %s: %w", planID, err)
	}

	return c, nil
}

// PlanByID reads a plan for an outbound call.
//
// An outbound call has no dialled DID to resolve — the number is the
// customer's — so the plan is named by the originate request rather than found
// from a number.
func (p *Pool) PlanByID(ctx context.Context, planID string) (route.Plan, error) {
	var pl route.Plan

	err := p.QueryRow(ctx, `
		SELECT id, tenant_id, destination, recording
		  FROM inbound_plans
		 WHERE id = $1 AND status = 'active' AND deleted_at IS NULL`, planID,
	).Scan(&pl.ID, &pl.TenantID, &pl.Destination, &pl.Recording)

	if errors.Is(err, pgx.ErrNoRows) {
		return route.Plan{}, ErrNotFound
	}
	if err != nil {
		return route.Plan{}, fmt.Errorf("store: getting plan %s: %w", planID, err)
	}
	return pl, nil
}
