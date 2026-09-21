// Package httpserver wraps net/http with the shutdown behaviour both services
// need: serve until the context is cancelled, then stop accepting and let
// in-flight requests finish.
package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Server is one listener with a name, so a failure says which one.
type Server struct {
	name string
	log  *slog.Logger
	srv  *http.Server

	// shutdownTimeout bounds the graceful stop. These are ordinary HTTP
	// requests measured in milliseconds; the long grace period in the systemd
	// unit is for the ESL sockets, not for these.
	shutdownTimeout time.Duration
}

// New builds a server. ReadHeaderTimeout is set because leaving it unset lets
// a client hold a connection open indefinitely by sending headers slowly, and
// the default is no timeout at all.
func New(name, addr string, h http.Handler, log *slog.Logger) *Server {
	return &Server{
		name: name,
		log:  log,
		srv: &http.Server{
			Addr:              addr,
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       120 * time.Second,
		},
		shutdownTimeout: 15 * time.Second,
	}
}

// Run serves until ctx is cancelled or the listener fails.
//
// http.ErrServerClosed is the ordinary outcome of a shutdown, not an error —
// returning it would make every clean stop look like a failure to whatever is
// watching the process exit.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)

	go func() {
		s.log.Info("http listening", "listener", s.name, "addr", s.srv.Addr)
		err := s.srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()

		s.log.Info("http stopping", "listener", s.name)
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return <-errCh
	}
}
