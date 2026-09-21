// Package run holds the process lifecycle: several concurrent runners, one
// signal handler, one shutdown.
//
// # Why SIGTERM handling is not optional
//
// The systemd unit gives esl-controller a 120 second TimeoutStopSec. That
// number is worth exactly nothing unless the process handles the signal — if
// it ignores SIGTERM, the unit waits two minutes and then SIGKILLs it, which
// is a slower version of the same abrupt death. The grace period is the
// service's chance to close its sockets and flush state, and only the service
// can take it.
//
// It is NOT a drain. With one app instance and no load balancer there is
// nowhere to move traffic to, so stopping the controller ends the calls it is
// carrying. The window exists so those calls end cleanly rather than
// mid-write.
package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Runner is one long-lived component: an HTTP listener, the ESL acceptor, a
// background sweeper. It returns when ctx is cancelled, or earlier on failure.
type Runner func(ctx context.Context) error

// App supervises runners.
type App struct {
	log      *slog.Logger
	runners  []namedRunner
	onDrain  []func()
	stopWait time.Duration
}

type namedRunner struct {
	name string
	fn   Runner
}

// New builds an App. stopWait is how long to wait for runners to return after
// the shutdown is triggered; pass something below the unit's TimeoutStopSec so
// the process exits on its own terms rather than being killed.
func New(log *slog.Logger, stopWait time.Duration) *App {
	return &App{log: log, stopWait: stopWait}
}

// Add registers a runner.
func (a *App) Add(name string, fn Runner) {
	a.runners = append(a.runners, namedRunner{name: name, fn: fn})
}

// OnShutdown registers work to do the moment a shutdown begins, before the
// runners are asked to stop — flipping health to unhealthy, for instance.
func (a *App) OnShutdown(fn func()) {
	a.onDrain = append(a.onDrain, fn)
}

// Run starts every runner and blocks.
//
// The first runner to return stops the rest: a service whose ESL listener has
// died is not usefully alive because its health endpoint is still up. It
// should exit and let systemd restart it.
func (a *App) Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	for _, r := range a.runners {
		wg.Add(1)
		go func(r namedRunner) {
			defer wg.Done()
			err := r.fn(runCtx)

			mu.Lock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", r.name, err))
			}
			mu.Unlock()

			// Whatever happened, this component is finished. Bring the rest
			// down with it rather than running half a service.
			cancel()
		}(r)
	}

	<-runCtx.Done()

	if ctx.Err() != nil {
		a.log.Info("signal received, shutting down", "grace", a.stopWait.String())
	}

	for _, fn := range a.onDrain {
		fn()
	}

	// Stop listening for further signals. A second Ctrl-C now kills the
	// process immediately, which is the behaviour someone pressing it twice
	// is asking for.
	stop()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		a.log.Info("stopped cleanly")
	case <-time.After(a.stopWait):
		// Say so explicitly. Exiting quietly here hides the fact that
		// something was still holding state when the process went away.
		a.log.Error("shutdown timed out, exiting with work still in flight",
			"waited", a.stopWait.String())
	}

	return errors.Join(errs...)
}
