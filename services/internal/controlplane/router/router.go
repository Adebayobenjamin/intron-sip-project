// Package router wires the control-plane API.
//
// Every route that touches data is authenticated. The two exceptions are the
// OpenAPI spec and the Scalar UI, and the reasoning is in internal/openapi:
// they describe endpoint shapes rather than data, and requiring a token to
// read the documentation makes it useless to the person whose problem is
// working out how to get one.
//
// Health is a separate listener on its own port and does not appear here.
package router

import (
	"log/slog"
	"net/http"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/controlplane/auth"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/controlplane/handlers"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/controlplane/openapi"
)

// Deps is everything the API needs.
type Deps struct {
	Auth           *auth.Authenticator
	Config         handlers.ConfigHandler
	Route          handlers.Route
	TrafficConfigs handlers.TrafficConfigsHandler
	Trunks         handlers.TrunksHandler
	Originate      handlers.OriginateHandler
	Keys           handlers.KeysHandler
	Calls          handlers.CallsHandler
	Log            *slog.Logger
}

// New builds the API mux.
//
// Scopes are assigned per route rather than per resource: reads need `read`,
// writes need `write`, and anything that crosses tenants checks Platform()
// inside the handler because a scope cannot express "only for your own
// tenant".
func New(d Deps) http.Handler {
	mux := http.NewServeMux()

	get := func(pattern string, h http.HandlerFunc) {
		mux.Handle("GET "+pattern, d.Auth.Require(auth.ScopeRead, h))
	}
	write := func(method, pattern string, h http.HandlerFunc) {
		mux.Handle(method+" "+pattern, d.Auth.Require(auth.ScopeWrite, h))
	}

	// The description of this API, and a UI for it. Unauthenticated — see the
	// package comment.
	mux.HandleFunc("GET /openapi.json", openapi.Spec(d.Log))
	mux.HandleFunc("GET /docs", openapi.Docs())

	// Diagnostics. The first question when a client says calls are not
	// arriving, answered by the same code the call path uses.
	mux.Handle("GET /v1/route", d.Auth.Require(auth.ScopeRead, d.Route))

	// Tenants
	get("/v1/tenants", d.Config.ListTenants)
	write("POST", "/v1/tenants", d.Config.CreateTenant)
	write("PATCH", "/v1/tenants/{id}", d.Config.UpdateTenant)
	write("DELETE", "/v1/tenants/{id}", d.Config.DeleteTenant)

	// Traffic configs — lines: a trunk's numbers, a direction, a destination,
	// a recording policy.
	get("/v1/traffic-configs", d.TrafficConfigs.List)
	write("POST", "/v1/traffic-configs", d.TrafficConfigs.Create)
	write("PATCH", "/v1/traffic-configs/{id}", d.TrafficConfigs.Update)
	write("DELETE", "/v1/traffic-configs/{id}", d.TrafficConfigs.Delete)

	// One number: which trunk carries it, which line handles it each way.
	get("/v1/numbers/{e164}", d.TrafficConfigs.Number)

	// Trunks
	get("/v1/trunks", d.Trunks.ListTrunks)
	write("POST", "/v1/trunks", d.Trunks.CreateTrunk)
	write("PATCH", "/v1/trunks/{id}", d.Trunks.UpdateTrunk)
	write("DELETE", "/v1/trunks/{id}", d.Trunks.DeleteTrunk)

	// How a trunk's numbers are divided among lines, and which are uncovered.
	get("/v1/trunks/{id}/coverage", d.TrafficConfigs.Coverage)

	// Handoff targets
	get("/v1/handoff-targets", d.Trunks.ListHandoffTargets)
	write("POST", "/v1/handoff-targets", d.Trunks.CreateHandoffTarget)
	write("PATCH", "/v1/handoff-targets/{id}", d.Trunks.UpdateHandoffTarget)
	write("DELETE", "/v1/handoff-targets/{id}", d.Trunks.DeleteHandoffTarget)

	// API keys. Creating one is an admin action: a write key that could mint
	// another key could mint one with more scope than it has.
	mux.Handle("GET /v1/api-keys", d.Auth.RequireFunc(auth.ScopeRead, d.Keys.List))
	mux.Handle("POST /v1/api-keys", d.Auth.RequireFunc(auth.ScopeAdmin, d.Keys.Create))
	mux.Handle("DELETE /v1/api-keys/{id}", d.Auth.RequireFunc(auth.ScopeAdmin, d.Keys.Revoke))

	// Outbound. The API queues; the controller dials.
	write("POST", "/v1/originate", d.Originate.Create)
	get("/v1/originate/{call_uuid}", d.Originate.Get)

	// Read models
	get("/v1/calls", d.Calls.Live)
	get("/v1/call-history", d.Calls.History)

	// Anything else. A 404 with a hint rather than Go's bare "404 page not
	// found", which on a JSON API reads like the wrong service entirely.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no such endpoint; see /docs"}` + "\n"))
	})

	return mux
}
