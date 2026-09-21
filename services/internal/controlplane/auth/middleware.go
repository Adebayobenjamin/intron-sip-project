package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

type contextKey struct{}

// FromContext returns the authenticated caller.
//
// The second return is false when the handler was reached without the
// middleware, which is a wiring mistake rather than an auth failure. Handlers
// must treat it as fatal rather than falling back to an empty Principal — an
// empty TenantID means "platform-wide" and would hand an unauthenticated
// request every tenant's data.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(contextKey{}).(Principal)
	return p, ok
}

// Require wraps a handler so only authenticated callers holding scope reach it.
func (a *Authenticator) Require(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.Authenticate(r.Context(), r)
		if err != nil {
			a.deny(w, r, err)
			return
		}

		if !p.Can(scope) {
			// 403, not 401: the caller is authenticated and simply may not do
			// this. Answering 401 would invite a client to retry with the same
			// credentials forever.
			writeError(w, http.StatusForbidden,
				"this key does not hold the "+scope+" scope")
			return
		}

		ctx := context.WithValue(r.Context(), contextKey{}, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireFunc is Require for a plain handler function.
func (a *Authenticator) RequireFunc(scope string, next http.HandlerFunc) http.Handler {
	return a.Require(scope, next)
}

// deny answers a failed authentication.
//
// The response says which of "no token" and "bad token" it was, because that
// distinction costs an attacker nothing — they know whether they sent one —
// and saves an operator the first ten minutes of every support conversation.
// It never echoes the token.
func (a *Authenticator) deny(w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="intron"`)

	switch {
	case errors.Is(err, ErrNoToken):
		writeError(w, http.StatusUnauthorized,
			"missing bearer token: send Authorization: Bearer <key>")

	case errors.Is(err, ErrExpired):
		writeError(w, http.StatusUnauthorized, "this key is expired or revoked")

	case errors.Is(err, ErrUnknownToken):
		writeError(w, http.StatusUnauthorized, "unrecognised key")

	default:
		// The lookup itself failed — the database is unreachable. NOT 401:
		// telling a caller their key is bad when the truth is that we cannot
		// check it sends them to rotate a credential that was fine.
		slog.Default().Error("authentication lookup failed",
			"path", r.URL.Path, "error", err)
		writeError(w, http.StatusServiceUnavailable,
			"cannot verify credentials right now")
	}
}

type errorBody struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(errorBody{Error: msg})
}
