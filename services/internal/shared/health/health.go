// Package health serves the endpoint the Ansible role checks after a deploy.
//
// # This endpoint asserts DEPENDENCIES, not liveness
//
// Both the Terraform variable and the Ansible template say so, and they are
// right: a check that only proves the process is running keeps a wedged node
// in service, and every call routed to it is dropped. So `control-plane`
// checks Postgres, and `esl-controller` checks Postgres and at least one
// FreeSWITCH node's inbound ESL.
//
// The controller does NOT check the control-plane API. It does not need that
// API to carry a call, and checking it would make the controller report
// unhealthy for a dependency it does not have.
//
// # Draining
//
// SetDraining(true) makes the endpoint fail while in-flight calls finish. There
// is no load balancer to drain to at one instance — this exists so a human or a
// script watching the endpoint sees the shutdown, rather than a service that
// claims to be healthy right up to the moment it stops answering.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// CheckFunc reports whether one dependency is usable. A nil error means usable.
type CheckFunc func(ctx context.Context) error

// checkTimeout bounds the whole endpoint. A health check that hangs is
// indistinguishable from a service that hangs, and both should read as
// unhealthy rather than as a request that never returns.
const checkTimeout = 3 * time.Second

// Registry holds the checks for one service.
type Registry struct {
	mu       sync.RWMutex
	checks   []namedCheck
	draining bool
}

type namedCheck struct {
	name string
	fn   CheckFunc
}

// New returns an empty registry. A registry with no checks reports healthy —
// which is correct only for a service with no dependencies, and neither of
// these is one. Register something.
func New() *Registry {
	return &Registry{}
}

// Register adds a dependency check. Order is preserved so the response reads
// the same every time.
func (r *Registry) Register(name string, fn CheckFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks = append(r.checks, namedCheck{name: name, fn: fn})
}

// SetDraining flips the endpoint to unhealthy without touching the checks.
func (r *Registry) SetDraining(d bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.draining = d
}

type response struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// Handler serves the health response: 200 when every check passes, 503
// otherwise, with a body naming which check failed and why.
//
// The body matters. A bare 503 tells whoever is looking that something is
// wrong and nothing about what, which on a box reachable only through SSM is
// the difference between a minute and an afternoon.
func (r *Registry) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		r.mu.RLock()
		checks := make([]namedCheck, len(r.checks))
		copy(checks, r.checks)
		draining := r.draining
		r.mu.RUnlock()

		out := response{Status: "ok", Checks: map[string]string{}}

		if draining {
			out.Status = "draining"
		}

		ctx, cancel := context.WithTimeout(req.Context(), checkTimeout)
		defer cancel()

		healthy := !draining
		for _, c := range checks {
			if err := c.fn(ctx); err != nil {
				out.Checks[c.name] = err.Error()
				healthy = false
				continue
			}
			out.Checks[c.name] = "ok"
		}

		if !healthy && out.Status == "ok" {
			out.Status = "unhealthy"
		}

		code := http.StatusOK
		if !healthy {
			code = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(out)
	}
}
