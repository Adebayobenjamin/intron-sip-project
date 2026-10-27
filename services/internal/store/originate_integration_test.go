//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
)

func seedOutbound(t *testing.T, pool *Pool) {
	t.Helper()

	mustExec(t, pool, `INSERT INTO tenants (id, name) VALUES ('t1', 'One')`)
	mustExec(t, pool, `
		INSERT INTO trunks (id, slug, name, carrier, sip_auth_method, sip_host, did_ranges)
		VALUES (7, 'twilio', 'Twilio', 'twilio', 'ip_whitelisting',
		        'intron-beta.pstn.dublin.twilio.com', '{[4930123400,4930123500)}')`)
	mustExec(t, pool, `
		INSERT INTO traffic_configs (tenant_id, trunk_id, name, direction, did_ranges, destination, recording_mode)
		VALUES ('t1', 7, 'Campaign', 'outbound', '{[4930123450,4930123460)}', 'ai', 'full')`)
}

func queued(uuid string) calls.OriginateRequest {
	return calls.OriginateRequest{
		CallUUID: uuid,
		TenantID: "t1",
		FromDID:  "+4930123456",
		ToNumber: "+2348012345678",
		TrunkID:  7,
	}
}

func TestQueueOriginate_StartsPending(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedOutbound(t, pool)

	r, err := pool.QueueOriginate(ctx, queued("c-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Status != calls.OriginatePending {
		t.Errorf("Status = %q, want pending", r.Status)
	}
}

// The claim carries the trunk host, joined in the same statement. Fetching it
// separately would let the trunk change between the two reads.
func TestClaimOriginate_CarriesTheTrunkHost(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedOutbound(t, pool)

	if _, err := pool.QueueOriginate(ctx, queued("c-1")); err != nil {
		t.Fatal(err)
	}

	r, ok, err := pool.ClaimOriginate(ctx, "node-1")
	if err != nil || !ok {
		t.Fatalf("claim failed: ok=%v err=%v", ok, err)
	}
	if r.TrunkHost != "intron-beta.pstn.dublin.twilio.com" {
		t.Errorf("TrunkHost = %q", r.TrunkHost)
	}
	if r.ClaimedBy != "node-1" {
		t.Errorf("ClaimedBy = %q, want node-1", r.ClaimedBy)
	}
}

// An empty queue is the ordinary case, hundreds of times a minute. It must be
// "nothing to do", not an error.
func TestClaimOriginate_EmptyQueueIsNotAnError(t *testing.T) {
	pool := migratedPool(t)

	_, ok, err := pool.ClaimOriginate(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("an empty queue must not error: %v", err)
	}
	if ok {
		t.Error("claimed something from an empty queue")
	}
}

// A claimed row must not be claimed again — otherwise a second poller places
// the same call, which means ringing a customer twice and billing for both.
func TestClaimOriginate_ClaimedRowIsNotReclaimed(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedOutbound(t, pool)

	if _, err := pool.QueueOriginate(ctx, queued("c-1")); err != nil {
		t.Fatal(err)
	}

	if _, ok, _ := pool.ClaimOriginate(ctx, "node-1"); !ok {
		t.Fatal("first claim failed")
	}
	if _, ok, _ := pool.ClaimOriginate(ctx, "node-2"); ok {
		t.Error("the same request was claimed twice")
	}
}

// Oldest first, so a request does not sit behind ones queued after it.
func TestClaimOriginate_TakesTheOldestFirst(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedOutbound(t, pool)

	for _, id := range []string{"c-1", "c-2", "c-3"} {
		if _, err := pool.QueueOriginate(ctx, queued(id)); err != nil {
			t.Fatal(err)
		}
	}

	for _, want := range []string{"c-1", "c-2", "c-3"} {
		r, ok, err := pool.ClaimOriginate(ctx, "node-1")
		if err != nil || !ok {
			t.Fatalf("claim failed: %v", err)
		}
		if r.CallUUID != want {
			t.Errorf("claimed %q, want %q", r.CallUUID, want)
		}
	}
}

// THE TEST THE QUEUE TURNS ON.
//
// FOR UPDATE SKIP LOCKED is what lets two controllers poll at once: each gets
// a different row and neither waits on the other. Without SKIP LOCKED the
// second poller BLOCKS on the first's row, then claims it after the commit —
// placing the same call twice.
//
// Every request must be claimed exactly once, by exactly one worker.
func TestClaimOriginate_ConcurrentWorkersNeverDoubleClaim(t *testing.T) {
	ctx := context.Background()

	cfg := testConfig(newTestDB(t))
	cfg.MaxConns = 20
	if err := Migrate(ctx, cfg, quietLogger()); err != nil {
		t.Fatal(err)
	}
	pool, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	seedOutbound(t, pool)

	const requests = 40
	for i := range requests {
		if _, err := pool.QueueOriginate(ctx, queued(fmt.Sprintf("c-%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	var (
		mu     sync.Mutex
		claims = map[string]string{} // call uuid -> worker
		wg     sync.WaitGroup
		start  = make(chan struct{})
	)

	for w := range 8 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start

			name := fmt.Sprintf("node-%d", worker)
			for {
				r, ok, err := pool.ClaimOriginate(ctx, name)
				if err != nil {
					t.Errorf("worker %s: %v", name, err)
					return
				}
				if !ok {
					return
				}

				mu.Lock()
				if prev, dup := claims[r.CallUUID]; dup {
					t.Errorf("%s was claimed twice: by %s and %s", r.CallUUID, prev, name)
				}
				claims[r.CallUUID] = name
				mu.Unlock()
			}
		}(w)
	}

	close(start)
	wg.Wait()

	if len(claims) != requests {
		t.Errorf("claimed %d of %d requests", len(claims), requests)
	}
}

// The reason is stored, not only logged: "why did that call never happen" is
// asked days later, by someone who cannot read the controller's journal.
func TestMarkOriginateFailed_KeepsTheReason(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedOutbound(t, pool)

	if _, err := pool.QueueOriginate(ctx, queued("c-1")); err != nil {
		t.Fatal(err)
	}
	if err := pool.MarkOriginateFailed(ctx, "c-1", "no trunk host"); err != nil {
		t.Fatal(err)
	}

	r, err := pool.GetOriginate(ctx, "c-1", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != calls.OriginateFailed {
		t.Errorf("Status = %q, want failed", r.Status)
	}
	if r.Error != "no trunk host" {
		t.Errorf("Error = %q", r.Error)
	}
}

func TestGetOriginate_IsTenantScoped(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedOutbound(t, pool)
	mustExec(t, pool, `INSERT INTO tenants (id, name) VALUES ('t2', 'Two')`)

	if _, err := pool.QueueOriginate(ctx, queued("c-1")); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.GetOriginate(ctx, "c-1", "t2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another tenant could read the request: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The from-number: one lookup, three answers
// ---------------------------------------------------------------------------

func TestResolveOutbound_ReturnsTheLineAndItsTrunk(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedOutbound(t, pool)

	l, err := pool.ResolveOutbound(ctx, "+4930123456", "t1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.TrafficConfigID == 0 || l.TrunkID != 7 || l.TrunkHost != "intron-beta.pstn.dublin.twilio.com" {
		t.Errorf("line = %+v", l)
	}
	// The recording mode travels with it, so the claim can snapshot it.
	if l.RecordingMode != "full" {
		t.Errorf("RecordingMode = %q, want full", l.RecordingMode)
	}
}

// The authorisation check. Without it anyone reaching the API can present any
// number on the platform, including another tenant's.
func TestResolveOutbound_RefusesAnotherTenantsNumber(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedOutbound(t, pool)
	mustExec(t, pool, `INSERT INTO tenants (id, name) VALUES ('t2', 'Two')`)

	if _, err := pool.ResolveOutbound(ctx, "+4930123456", "t2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a tenant presented another tenant's number: %v", err)
	}
}

func TestResolveOutbound_RefusesAnInactiveLine(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedOutbound(t, pool)
	mustExec(t, pool, `UPDATE traffic_configs SET status = 'inactive'`)

	if _, err := pool.ResolveOutbound(ctx, "+4930123456", "t1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an inactive line was accepted for a caller ID: %v", err)
	}
}

// A number on the trunk but outside the outbound line — or inside only an
// INBOUND one — is not a number the tenant may dial out from.
func TestResolveOutbound_OnlyOutboundLinesCount(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedOutbound(t, pool)
	mustExec(t, pool, `
		INSERT INTO traffic_configs (tenant_id, trunk_id, name, direction, did_ranges, destination)
		VALUES ('t1', 7, 'Main', 'inbound', '{[4930123400,4930123410)}', 'ai')`)

	if _, err := pool.ResolveOutbound(ctx, "+4930123405", "t1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an inbound-only number was accepted as an outbound caller ID: %v", err)
	}
}
