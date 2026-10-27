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

// A number arrives on exactly one trunk. Without the exclusion constraint two
// trunks could claim it, and which carrier — and so which tenant — a call is
// attributed to would depend on row order.
func TestSchema_NumberIsOnOneTrunkOnly(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	insert := `INSERT INTO trunks (slug, name, carrier, sip_auth_method, did_ranges)
	           VALUES ($1, $1, 'twilio', 'ip_whitelisting', $2::int8multirange)`

	mustExec(t, pool, insert, "twilio", "{[2348000000100,2348000000200)}")

	if _, err := pool.Exec(ctx, insert, "other", "{[2348000000199,2348000000300)}"); err == nil {
		t.Fatal("want an exclusion violation when two trunks carry one number")
	}

	// A soft-deleted trunk must not block re-allocating its numbers: a block
	// moved from one carrier to another is an ordinary event.
	mustExec(t, pool, `UPDATE trunks SET deleted_at = now() WHERE slug = 'twilio'`)
	mustExec(t, pool, insert, "other", "{[2348000000199,2348000000300)}")
}

// A line destination outside the three the controller knows how to act on
// would be a call the controller cannot route, discovered at call time.
func TestSchema_LineDestinationIsConstrained(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	mustExec(t, pool, `INSERT INTO tenants (id, name) VALUES ('t1', 'One')`)
	mustExec(t, pool, `INSERT INTO trunks (id, slug, name, carrier, sip_auth_method, did_ranges)
	                   VALUES (1, 'x', 'X', 'x', 'ip_whitelisting', '{[100,200)}')`)

	insert := `INSERT INTO traffic_configs (tenant_id, trunk_id, name, direction, did_ranges, destination)
	           VALUES ('t1', 1, 'Main', 'inbound', '{[100,150)}', $1)`

	if _, err := pool.Exec(ctx, insert, "voicemail"); err == nil {
		t.Fatal("want a check violation for an unknown destination")
	}
	mustExec(t, pool, insert, "ai")
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
