package store

import (
	"context"
	"fmt"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/route"
)

// HandoffTargets returns where a call goes when it needs a human, in the order
// they should be tried.
//
// # Which list
//
// The line's overrides if it has any enabled, otherwise the tenant's default
// list. Overrides REPLACE the default rather than adding to it: "this line goes
// to the Lagos queue" is a statement that the default queue is the wrong place
// for it.
//
// # Which trunk an E.164 target leaves by
//
// The CALL'S trunk — the one it arrived on, or for an outbound call the one it
// left by — passed in by the caller from the call's own record.
//
// This used to join "the tenant's default trunk", which does not exist for a
// tenant on the platform trunk: the platform trunk has no tenant, so the join
// found nothing and every E.164 handoff for those tenants failed with "the
// tenant has no default trunk". Using the call's trunk is also simply right:
// it is the carrier the line's numbers live on, so it is the account the
// handoff call can be placed through.
//
// Ordered by priority then id. The id tiebreak is not decoration: two targets
// at the same priority would otherwise come back in whatever order the planner
// chose, so the same call could take a different route on two identical
// attempts and nobody could reproduce a report about it.
func (p *Pool) HandoffTargets(ctx context.Context, tenantID string, trafficConfigID, trunkID int64) ([]route.HandoffTarget, error) {
	rows, err := p.Query(ctx, `
		WITH overrides AS (
		    SELECT EXISTS (
		        SELECT 1 FROM handoff_targets
		         WHERE traffic_config_id = $2
		           AND tenant_id = $1
		           AND enabled
		           AND deleted_at IS NULL) AS present
		), trunk AS (
		    SELECT COALESCE(sip_host, '') AS host, sip_port AS port
		      FROM trunks
		     WHERE id = $3 AND deleted_at IS NULL
		)
		SELECT h.id, h.kind, h.destination,
		       COALESCE((SELECT host FROM trunk), ''),
		       COALESCE((SELECT port FROM trunk), 0)
		  FROM handoff_targets h, overrides o
		 WHERE h.tenant_id = $1
		   AND h.enabled
		   AND h.deleted_at IS NULL
		   AND CASE WHEN o.present THEN h.traffic_config_id = $2
		            ELSE h.traffic_config_id IS NULL END
		 ORDER BY h.priority, h.id`,
		tenantID, nullInt64Arg(trafficConfigID), nullInt64Arg(trunkID))
	if err != nil {
		return nil, fmt.Errorf("store: loading handoff targets for %s: %w", tenantID, err)
	}
	defer rows.Close()

	var out []route.HandoffTarget
	for rows.Next() {
		var t route.HandoffTarget
		if err := rows.Scan(&t.ID, &t.Kind, &t.Destination, &t.TrunkHost, &t.TrunkPort); err != nil {
			return nil, fmt.Errorf("store: scanning handoff target: %w", err)
		}
		out = append(out, t)
	}

	return out, rows.Err()
}
