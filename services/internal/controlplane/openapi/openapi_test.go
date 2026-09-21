package openapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// serve returns the spec as it would be served to a request with this host and
// these headers.
func serve(t *testing.T, host string, headers map[string]string) map[string]any {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	r.Host = host
	for k, v := range headers {
		r.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	Spec(quiet())(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	return body
}

func firstServerURL(t *testing.T, body map[string]any) string {
	t.Helper()

	servers, ok := body["servers"].([]any)
	if !ok || len(servers) == 0 {
		t.Fatalf("no servers in the spec: %v", body["servers"])
	}
	s, _ := servers[0].(map[string]any)
	url, _ := s["url"].(string)
	return url
}

// The whole point: a reader on an SSM port-forward gets a "Try it" button that
// targets the tunnel, not an internal address their browser cannot resolve.
func TestSpec_FirstServerIsTheRequestingHost(t *testing.T) {
	body := serve(t, "127.0.0.1:18080", nil)

	if got := firstServerURL(t, body); got != "http://127.0.0.1:18080" {
		t.Errorf("first server = %q, want the tunnel", got)
	}
}

// The real internal address stays in the list, so it is still discoverable.
func TestSpec_KeepsTheDocumentedServers(t *testing.T) {
	body := serve(t, "127.0.0.1:18080", nil)

	servers, _ := body["servers"].([]any)
	if len(servers) < 2 {
		t.Fatalf("want the documented servers kept after the current host, got %d", len(servers))
	}

	var found bool
	for _, s := range servers[1:] {
		if m, ok := s.(map[string]any); ok {
			if u, _ := m["url"].(string); u == "http://api.beta.intron.internal:8080" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the documented internal address was dropped: %v", servers)
	}
}

// Behind a TLS-terminating load balancer, r.TLS is nil here. Reporting http
// would produce a mixed-content failure in the browser rather than a working
// button.
func TestSpec_HonoursForwardedProto(t *testing.T) {
	body := serve(t, "api.example.com", map[string]string{
		"X-Forwarded-Proto": "https",
	})

	if got := firstServerURL(t, body); got != "https://api.example.com" {
		t.Errorf("first server = %q, want https", got)
	}
}

func TestSpec_HonoursForwardedHost(t *testing.T) {
	body := serve(t, "10.70.10.5:8080", map[string]string{
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "api.intron.example",
	})

	if got := firstServerURL(t, body); got != "https://api.intron.example" {
		t.Errorf("first server = %q, want the forwarded host", got)
	}
}

// The rewrite must not damage the rest of the document.
func TestSpec_RemainsAValidDocument(t *testing.T) {
	body := serve(t, "127.0.0.1:18080", nil)

	if body["openapi"] != "3.1.0" {
		t.Errorf("openapi = %v", body["openapi"])
	}

	paths, ok := body["paths"].(map[string]any)
	if !ok || len(paths) < 15 {
		t.Errorf("paths lost in the rewrite: %d", len(paths))
	}
	if _, ok := paths["/v1/originate"]; !ok {
		t.Error("/v1/originate is missing")
	}

	comps, ok := body["components"].(map[string]any)
	if !ok {
		t.Fatal("components missing")
	}
	if _, ok := comps["securitySchemes"]; !ok {
		t.Error("securitySchemes missing — the docs would not offer an auth box")
	}
}

// Two readers on different hosts must not be served each other's origin, which
// is why the response is Cache-Control: private.
func TestSpec_IsNotSharedCacheable(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	r.Host = "127.0.0.1:18080"

	rec := httptest.NewRecorder()
	Spec(quiet())(rec, r)

	if cc := rec.Header().Get("Cache-Control"); cc == "" || cc[:7] != "private" {
		t.Errorf("Cache-Control = %q; the body varies by Host so it must not be shared", cc)
	}
}

// The page must reference the spec RELATIVELY, or a tunnelled reader is sent
// to an address they cannot reach.
func TestDocs_ReferencesTheSpecRelatively(t *testing.T) {
	rec := httptest.NewRecorder()
	Docs()(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))

	page := rec.Body.String()
	if !contains(page, "url: 'openapi.json'") {
		t.Errorf("the spec URL should be relative: %s", page)
	}
	if contains(page, "intron.internal") {
		t.Error("the page hardcodes an internal address")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}
