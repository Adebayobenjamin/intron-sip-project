//go:build integration

// Integration tests for the schema and the deploy-order guard.
//
// Tagged, because they need a real Postgres — the behaviour under test is
// mostly behaviour Postgres provides (constraints, partial unique indexes,
// migration bookkeeping), and a mock would only assert that the mock works.
//
//	make test-integration
//
// Set TEST_DATABASE_URL to point somewhere other than the default. Each test
// creates and drops its own database, so they do not interfere.
package store

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func adminURL() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://localhost:5432/postgres?sslmode=disable"
}

// newTestDB creates a throwaway database and returns its URL.
func newTestDB(t *testing.T) string {
	t.Helper()

	ctx := context.Background()
	name := fmt.Sprintf("intron_test_%d", time.Now().UnixNano())

	admin, err := pgx.Connect(ctx, adminURL())
	if err != nil {
		t.Skipf("no Postgres at %s: %v", adminURL(), err)
	}
	defer admin.Close(ctx)

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating test database: %v", err)
	}

	t.Cleanup(func() {
		cleanCtx := context.Background()
		a, err := pgx.Connect(cleanCtx, adminURL())
		if err != nil {
			return
		}
		defer a.Close(cleanCtx)
		_, _ = a.Exec(cleanCtx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	return strings.Replace(adminURL(), "/postgres?", "/"+name+"?", 1)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig(url string) Config {
	return Config{URL: url, MaxConns: 4, AppName: "store-test"}
}

func TestMigrate_AppliesAndIsIdempotent(t *testing.T) {
	url := newTestDB(t)
	ctx := context.Background()
	cfg := testConfig(url)

	if err := Migrate(ctx, cfg, quietLogger()); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	// Running again must be a no-op, not an error. Every service start calls
	// this, so a second run failing would mean a service that only starts once.
	if err := Migrate(ctx, cfg, quietLogger()); err != nil {
		t.Fatalf("second migrate should be a no-op, got: %v", err)
	}

	pool, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	defer pool.Close()

	if err := pool.AssertSchema(ctx); err != nil {
		t.Errorf("AssertSchema after migrating: %v", err)
	}
}

// The deploy-order guard. esl-controller must refuse to start against a
// database control-plane has not migrated, rather than starting and failing
// one call at a time — which reads as an intermittent fault rather than a
// deploy done in the wrong order.
func TestAssertSchema_RefusesUnmigratedDatabase(t *testing.T) {
	url := newTestDB(t)
	ctx := context.Background()

	pool, err := Open(ctx, testConfig(url))
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	defer pool.Close()

	err = pool.AssertSchema(ctx)
	if err == nil {
		t.Fatal("want an error against an unmigrated database, got nil")
	}
	if !strings.Contains(err.Error(), "control-plane") {
		t.Errorf("the error should point at the cause, got: %v", err)
	}
}

func TestOpen_RequiresExplicitMaxConns(t *testing.T) {
	_, err := Open(context.Background(), Config{URL: adminURL()})
	if err == nil {
		t.Fatal("want an error when MaxConns is unset")
	}
	if !strings.Contains(err.Error(), "MaxConns") {
		t.Errorf("error should name MaxConns, got: %v", err)
	}
}

// A number belongs to exactly one tenant. Without the partial unique index,
// two tenants can claim the same DID and which one answers depends on row
// order — calls occasionally reaching the wrong customer.
func TestSchema_DIDIsUniqueAcrossTenants(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	mustExec(t, pool, `INSERT INTO tenants (id, name) VALUES ('t1', 'One'), ('t2', 'Two')`)
	mustExec(t, pool, `INSERT INTO dids (number, tenant_id) VALUES ('+4930123456', 't1')`)

	_, err := pool.Exec(ctx,
		`INSERT INTO dids (number, tenant_id) VALUES ('+4930123456', 't2')`)
	if err == nil {
		t.Fatal("want a unique violation when two tenants claim one number")
	}

	// A soft-deleted row must not block re-allocating the number: a DID
	// released by one tenant and sold to another is an ordinary event.
	mustExec(t, pool, `UPDATE dids SET deleted_at = now() WHERE number = '+4930123456'`)
	mustExec(t, pool, `INSERT INTO dids (number, tenant_id) VALUES ('+4930123456', 't2')`)
}

// Two default trunks would make egress routing depend on row order.
func TestSchema_OnlyOneDefaultTrunk(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	insert := `INSERT INTO trunks (slug, name, type, carrier, sip_auth_method, is_default)
	           VALUES ($1, $1, 'bi_directional', 'twilio', 'ip_whitelisting', true)`

	mustExec(t, pool, insert, "twilio-frankfurt")

	if _, err := pool.Exec(ctx, insert, "twilio-dublin"); err == nil {
		t.Fatal("want a unique violation on a second default trunk")
	}
}

// A plan destination outside the three the controller knows how to act on
// would be a call the controller cannot route, discovered at call time.
func TestSchema_PlanDestinationIsConstrained(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	mustExec(t, pool, `INSERT INTO tenants (id, name) VALUES ('t1', 'One')`)

	_, err := pool.Exec(ctx,
		`INSERT INTO inbound_plans (id, tenant_id, name, destination)
		 VALUES ('p1', 't1', 'Main', 'voicemail')`)
	if err == nil {
		t.Fatal("want a check violation for an unknown destination")
	}

	mustExec(t, pool,
		`INSERT INTO inbound_plans (id, tenant_id, name, destination)
		 VALUES ('p1', 't1', 'Main', 'ai')`)
}

func migratedPool(t *testing.T) *Pool {
	t.Helper()

	ctx := context.Background()
	cfg := testConfig(newTestDB(t))

	if err := Migrate(ctx, cfg, quietLogger()); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	pool, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

func mustExec(t *testing.T, pool *Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
