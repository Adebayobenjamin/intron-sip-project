package ctrl

import (
	"context"
	"log/slog"
	"time"
)

// Sweeper defaults.
const (
	// sweepInterval is how often stale claims are reclaimed. Frequent enough
	// that a crash costs minutes of capacity, rare enough to be invisible on
	// the database.
	sweepInterval = time.Minute

	// staleAfter is how long a claim may go without a heartbeat before it is
	// assumed abandoned. Several times the heartbeat interval, so a slow
	// database or a paused process does not cost a live call its slot.
	staleAfter = 5 * time.Minute
)

// Sweeper reclaims concurrency slots whose controller stopped heartbeating.
//
// # Why this exists
//
// A row in `calls` is a claim on a tenant's concurrency. The controller
// deletes its rows when calls end, but a process that is killed — SIGKILL past
// the grace period, an instance that goes away — never gets to. Those rows
// stay claimed with nobody on the calls.
//
// Without a sweeper the effect is permanent and silent: the tenant's usable
// capacity drops by the number of calls that were in flight when the process
// died, and the symptom is calls refused for congestion on a platform that is
// carrying nothing. Nobody would look at a row count for that.
type Sweeper struct {
	store StaleSweeper
	log   *slog.Logger
}

// StaleSweeper is the store, narrowed.
type StaleSweeper interface {
	SweepStaleCalls(ctx context.Context, olderThan time.Duration) (int64, error)
}

// NewSweeper builds a Sweeper.
func NewSweeper(store StaleSweeper, log *slog.Logger) *Sweeper {
	return &Sweeper{store: store, log: log}
}

// Run sweeps until the context ends.
//
// It runs once at startup before waiting, deliberately. The most likely reason
// stale rows exist is the crash that this process is the restart of, so
// waiting a full interval would carry the reduced capacity into the first
// minute of service.
func (s *Sweeper) Run(ctx context.Context) error {
	s.sweep(ctx)

	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

func (s *Sweeper) sweep(ctx context.Context) {
	n, err := s.store.SweepStaleCalls(ctx, staleAfter)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.log.Error("sweep failed; stale slots remain claimed", "error", err)
		return
	}

	if n > 0 {
		// Always worth a line. A non-zero result means a controller died
		// holding calls, which is not otherwise visible anywhere.
		s.log.Warn("reclaimed abandoned concurrency slots",
			"slots", n, "stale_after", staleAfter.String())
	}
}
