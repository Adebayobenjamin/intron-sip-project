package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	// Registers the "pgx" driver with database/sql, which the migrator needs.
	// Blank import for a side effect — removing it compiles fine and fails at
	// runtime with "unknown driver".
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Adebayobenjamin/intron-sip-project/services/migrations"
)

// SchemaVersion is the migration this build expects. Defined next to the SQL
// so the two are bumped together.
const SchemaVersion = migrations.Version

// Migrate applies pending migrations. ONLY control-plane calls this.
//
// It opens its own short-lived database/sql connection rather than borrowing
// the pgxpool: golang-migrate wants a *sql.DB, and this runs once at startup.
// Going through WithInstance also sidesteps guessing which URL scheme the
// migrator has registered for which driver — a mismatch there fails with
// "unknown driver postgres", which sends people looking at the DSN.
func Migrate(ctx context.Context, cfg Config, log *slog.Logger) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("store: reading embedded migrations: %w", err)
	}
	defer src.Close()

	db, err := sql.Open("pgx", cfg.URL)
	if err != nil {
		// Does not include the URL: it carries the password, and this error
		// ends up in the journal.
		return fmt.Errorf("store: opening migration connection: %w", err)
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("store: connecting for migration: %w", err)
	}

	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		return fmt.Errorf("store: building migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		return fmt.Errorf("store: opening migrator: %w", err)
	}

	version, dirty, err := m.Version()
	switch {
	case errors.Is(err, migrate.ErrNilVersion):
		log.Info("no schema present, applying migrations from scratch")
	case err != nil:
		return fmt.Errorf("store: reading schema version: %w", err)
	case dirty:
		// A dirty version means a previous migration failed part way.
		// Applying more on top of an unknown state is how a half-applied
		// schema becomes a permanently confusing one. Stop and make a human
		// look.
		return fmt.Errorf(
			"store: schema is dirty at version %d — a previous migration failed "+
				"part way. Inspect the database, fix it by hand, then "+
				"`migrate force %d`", version, version)
	default:
		log.Info("schema present", "version", version)
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("store: applying migrations: %w", err)
	}

	final, _, err := m.Version()
	if err != nil {
		return fmt.Errorf("store: reading schema version after migrating: %w", err)
	}

	log.Info("schema ready", "version", final)

	if final != SchemaVersion {
		// The SQL files and the constant disagree: someone added a migration
		// and did not bump Version. Harmless now, misleading later, when the
		// controller's startup check passes against the wrong number.
		log.Warn("schema version does not match the value compiled into this build",
			"database", final, "expected", SchemaVersion)
	}

	return nil
}

// AssertSchema verifies the database is at the version this build expects,
// without changing anything. esl-controller calls this at startup.
//
// Refusing to start is the right response to a schema older than the code. The
// alternative is a controller that starts, queries a column that does not
// exist, and fails one call at a time — which reads as an intermittent fault
// rather than a deploy done in the wrong order.
func (p *Pool) AssertSchema(ctx context.Context) error {
	var (
		version int64
		dirty   bool
	)

	// golang-migrate's own bookkeeping table, read directly. Opening a
	// migrator here would need the URL again and could apply something by
	// accident, which is exactly what this service must not do.
	err := p.QueryRow(ctx,
		`SELECT version, dirty FROM schema_migrations LIMIT 1`,
	).Scan(&version, &dirty)
	if err != nil {
		return fmt.Errorf(
			"reading schema version (has control-plane run its migrations yet?): %w", err)
	}

	if dirty {
		return fmt.Errorf("schema is dirty at version %d; a migration failed part way", version)
	}

	if version < SchemaVersion {
		return fmt.Errorf(
			"schema is at version %d, this build needs %d — deploy control-plane first",
			version, SchemaVersion)
	}

	return nil
}
