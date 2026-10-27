// Package store owns the database: the connection pool, the migrations, and
// the SQL both services run.
//
// # Why both services import this
//
// esl-controller and control-plane read the same tables. Two hand-written
// copies of "resolve a number" drift — one gets the deleted_at filter and the
// other does not — and the difference surfaces as a call routed to a tenant
// removed last week. One package, two callers.
//
// # Who migrates
//
// control-plane, and only control-plane. Both hold a credential, but two
// processes racing golang-migrate at startup is a deploy-ordering problem
// nobody wants to debug at 3am. The controller calls AssertSchema instead,
// which checks the version it expects is present and refuses to start if it is
// not.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is the shared connection pool.
type Pool struct {
	*pgxpool.Pool
}

// Config is what Open needs. MaxConns is required rather than defaulted
// because the default is the problem — see below.
type Config struct {
	URL      string
	MaxConns int

	// AppName appears in pg_stat_activity, so two services sharing one small
	// instance can be told apart when something is holding connections.
	AppName string
}

// connectTimeout bounds startup. A service that hangs forever waiting for a
// database is indistinguishable from one that is merely slow to boot, and
// systemd will keep waiting rather than reporting a failure.
const connectTimeout = 10 * time.Second

// Open builds the pool and verifies it can actually reach the database.
//
// MAX CONNECTIONS ARE SET EXPLICITLY, and this is the one setting worth being
// awkward about. db.t4g.micro has 1 GiB of RAM, which is roughly 110 Postgres
// connections in TOTAL. pgx defaults to max(4, NumCPU), which is modest on a
// laptop and not on a larger instance — and there are two services sharing
// that ceiling, plus whatever an operator's psql session takes.
//
// Exhausting it fails as `FATAL: sorry, too many clients already` on whichever
// service started second. At that point neither the API nor calls work, for a
// reason that names nothing about instance memory.
func Open(ctx context.Context, cfg Config) (*Pool, error) {
	if cfg.MaxConns <= 0 {
		return nil, fmt.Errorf("store: MaxConns must be set explicitly, got %d", cfg.MaxConns)
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// Deliberately does not wrap the URL into the message: it carries the
		// password, and this error ends up in the journal.
		return nil, fmt.Errorf("store: parsing DATABASE_URL: %w", err)
	}

	poolCfg.MaxConns = int32(cfg.MaxConns)

	// Recycle idle connections rather than holding them open forever. RDS
	// drops long-idle connections its own way, and a pool that has not noticed
	// hands out a dead one to the next caller.
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.HealthCheckPeriod = 30 * time.Second

	if cfg.AppName != "" {
		if poolCfg.ConnConfig.RuntimeParams == nil {
			poolCfg.ConnConfig.RuntimeParams = map[string]string{}
		}
		poolCfg.ConnConfig.RuntimeParams["application_name"] = cfg.AppName
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("store: creating pool: %w", err)
	}

	// pgxpool is lazy — NewWithConfig does not connect. Without this ping a
	// service starts "successfully" against an unreachable database and only
	// discovers it on the first call.
	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: connecting: %w", err)
	}

	return &Pool{Pool: pool}, nil
}

// HealthCheck reports whether the database is usable right now.
//
// Ping rather than a trivial SELECT: it acquires a connection from the pool,
// so an exhausted pool reads as unhealthy. A `SELECT 1` on a connection the
// caller already holds proves less than it appears to.
func (p *Pool) HealthCheck(ctx context.Context) error {
	if p == nil || p.Pool == nil {
		return fmt.Errorf("no database pool")
	}
	return p.Ping(ctx)
}
