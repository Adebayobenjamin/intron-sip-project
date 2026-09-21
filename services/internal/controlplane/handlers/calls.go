package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/calls"
)

// CallsStore is the store, narrowed.
type CallsStore interface {
	LiveCalls(ctx context.Context, tenantID string) ([]calls.Live, error)
	CallHistory(ctx context.Context, tenantID string, limit int, before *time.Time) ([]calls.Historic, error)
}

// CallsHandler serves the read models.
type CallsHandler struct {
	Store CallsStore
	Log   *slog.Logger
}

// Live answers "what is happening right now".
func (h CallsHandler) Live(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}
	tenant, ok := scopeTenant(w, p, r.URL.Query().Get("tenant_id"))
	if !ok {
		return
	}

	live, err := h.Store.LiveCalls(r.Context(), tenant)
	if err != nil {
		failErr(w, h.Log, "list live calls", err)
		return
	}
	respond(w, http.StatusOK, live)
}

// History pages through finished calls.
//
// Paged by timestamp rather than offset. call_history grows forever and rows
// are inserted constantly, so an offset shifts under the reader between pages
// — which shows up as duplicated and skipped rows in an export nobody
// double-checks.
func (h CallsHandler) History(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r, h.Log)
	if !ok {
		return
	}
	tenant, ok := scopeTenant(w, p, r.URL.Query().Get("tenant_id"))
	if !ok {
		return
	}

	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			fail(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		// Capped. An unbounded limit is a way to ask this service to load
		// every call ever made into memory at once.
		limit = min(n, 1000)
	}

	var before *time.Time
	if raw := r.URL.Query().Get("before"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			fail(w, http.StatusBadRequest, "before must be an RFC3339 timestamp")
			return
		}
		before = &t
	}

	history, err := h.Store.CallHistory(r.Context(), tenant, limit, before)
	if err != nil {
		failErr(w, h.Log, "list call history", err)
		return
	}
	respond(w, http.StatusOK, history)
}
