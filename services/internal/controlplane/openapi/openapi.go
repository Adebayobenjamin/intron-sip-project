// Package openapi serves the API description and a Scalar UI for it.
//
// # Why these two routes are not authenticated
//
// Every other route on this mux requires a bearer token. These do not, and the
// exception is deliberate rather than an oversight:
//
//   - The spec describes endpoint SHAPES, not data. It reveals nothing a
//     caller with a token could not already see, and the service is reachable
//     only inside the VPC.
//   - Requiring a token to read the documentation makes it useless to the
//     person whose problem is working out how to get a token.
//
// The alternative — hiding the docs — buys nothing against anyone who has
// already reached the port.
package openapi

import (
	_ "embed"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"sync"
)

// specJSON is embedded so the binary and its description are one artefact.
//
// A spec served from a file on disk drifts from the code that ships with it,
// and an immutable image tag then names two different things depending on what
// is mounted.
//
//go:embed openapi.json
var specJSON []byte

var (
	parseOnce sync.Once
	parsed    map[string]any
	parseErr  error

	// documented are the servers written into openapi.json — the real
	// addresses, worth keeping so a reader learns where this actually lives.
	documented []any
)

func load() {
	parseOnce.Do(func() {
		parseErr = json.Unmarshal(specJSON, &parsed)
		if parseErr != nil {
			return
		}
		if s, ok := parsed["servers"].([]any); ok {
			documented = s
		}
	})
}

// Spec serves the OpenAPI document with the CURRENT host as its first server.
//
// # Why the host is rewritten per request
//
// The spec is written with the address this service actually has inside the
// VPC. But the API is private, so the usual way to reach it from a laptop is
// an SSM port-forward on some arbitrary local port — and a Scalar "Try it"
// button aimed at api.beta.intron.internal:8080 from a browser that cannot
// resolve it fails in a way that looks like the service is down.
//
// Answering with the origin the request arrived on makes the button work
// wherever the reader is: through a tunnel, over loopback on the box, or from
// a future load balancer. The documented addresses are kept after it, so the
// real one is still discoverable.
func Spec(log *slog.Logger) http.HandlerFunc {
	load()

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Short, not immutable: the spec changes with every deploy, and a
		// stale cached copy is how someone debugs against an endpoint that no
		// longer exists. Private, because the body now varies by Host and a
		// shared cache must not hand one reader another's origin.
		w.Header().Set("Cache-Control", "private, max-age=60")

		if parseErr != nil {
			// The embedded spec is malformed, which is a build-time mistake
			// rather than anything the reader did. Serve it verbatim rather
			// than nothing — an unpatched spec is still usable.
			log.Error("embedded openapi.json could not be parsed; serving it unmodified",
				"error", parseErr)
			_, _ = w.Write(specJSON)
			return
		}

		out := maps.Clone(parsed)
		out["servers"] = serversFor(r)

		if err := json.NewEncoder(w).Encode(out); err != nil {
			log.Error("writing openapi.json", "error", err)
		}
	}
}

// serversFor puts the requesting origin first.
func serversFor(r *http.Request) []any {
	origin := originOf(r)
	if origin == "" {
		return documented
	}

	servers := []any{map[string]any{
		"url":         origin,
		"description": "this host",
	}}
	return append(servers, documented...)
}

// originOf reconstructs the URL the reader used.
//
// X-Forwarded-Proto is honoured because the eventual answer to "can we reach
// this from outside" is a load balancer terminating TLS — at which point
// r.TLS is nil here and the scheme would otherwise be reported as http,
// producing a mixed-content failure in the browser rather than a working
// button.
func originOf(r *http.Request) string {
	if r.Host == "" {
		return ""
	}

	scheme := "http"
	switch {
	case r.Header.Get("X-Forwarded-Proto") != "":
		scheme = r.Header.Get("X-Forwarded-Proto")
	case r.TLS != nil:
		scheme = "https"
	}

	host := r.Host
	if f := r.Header.Get("X-Forwarded-Host"); f != "" {
		host = f
	}

	return scheme + "://" + host
}

// docsPage is Scalar, pointed at the spec above.
//
// Loaded from a CDN rather than vendored: the runtime image is distroless and
// ~5MB, and bundling a documentation UI into it would roughly double that to
// serve a page nobody opens from inside the VPC anyway. The cost is that the
// page needs internet access to render — the spec itself does not, and is the
// part that matters to a machine.
//
// The spec URL is RELATIVE, so the browser resolves it against whatever host
// the page was served from. An absolute one would send a tunnelled reader back
// to an address they cannot reach.
const docsPage = `<!doctype html>
<html>
  <head>
    <title>Intron control plane</title>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
  </head>
  <body>
    <div id="app"></div>
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
    <script>
      Scalar.createApiReference('#app', {
        url: 'openapi.json',
        theme: 'purple',
        hideDownloadButton: false,
      })
    </script>
  </body>
</html>
`

// Docs serves the Scalar reference UI.
func Docs() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(docsPage))
	}
}
