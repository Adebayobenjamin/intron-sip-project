// Package config loads service configuration from the environment.
//
// The environment is the ONLY source. Ansible's app-service role renders an env
// file from Secrets Manager at every service start and passes it to the
// container with --env-file, so by the time a process here runs, every value is
// already resolved. There is no Secrets Manager SDK in these binaries and no
// AWS call on the startup path — see roles/app-service/templates/service.env.tpl.j2.
//
// # Failing closed
//
// A missing required key is a startup failure, never a zero value. The secret
// fetch upstream already fails closed rather than starting a service with a
// literal ${PLACEHOLDER} as a password, and this matches it: a control plane
// that starts with an empty DATABASE_URL and then rejects every call is worse
// than one that does not start, because only the second is obvious.
//
// Load reports EVERY missing key at once. Discovering them one restart at a
// time, over SSM, on a box with no shell, is an afternoon.
package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// loader accumulates problems instead of returning on the first one.
type loader struct {
	missing []string
	invalid map[string]string
}

func newLoader() *loader {
	return &loader{invalid: map[string]string{}}
}

func (l *loader) required(key string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		l.missing = append(l.missing, key)
	}
	return v
}

func (l *loader) optional(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

func (l *loader) requiredInt(key string) int {
	raw := l.required(key)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		l.invalid[key] = fmt.Sprintf("want an integer, got %q", raw)
		return 0
	}
	return n
}

func (l *loader) optionalInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		l.invalid[key] = fmt.Sprintf("want an integer, got %q", raw)
		return def
	}
	return n
}

// optionalSeconds reads an integer count of seconds. The inventory expresses
// these as bare integers (esl_tcp_keepalive_seconds: 60), not Go durations.
func (l *loader) optionalSeconds(key string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		l.invalid[key] = fmt.Sprintf("want a whole number of seconds, got %q", raw)
		return def
	}
	return time.Duration(n) * time.Second
}

// requiredList reads a comma-separated list. FREESWITCH_NODES is rendered by
// Ansible as `{{ freeswitch_private_ips | join(',') }}`.
func (l *loader) requiredList(key string) []string {
	raw := l.required(key)
	if raw == "" {
		return nil
	}
	out := splitAndTrim(raw)
	if len(out) == 0 {
		l.invalid[key] = fmt.Sprintf("want a comma-separated list, got %q", raw)
	}
	return out
}

// splitAndTrim splits on commas and drops empties, so a trailing comma or a
// stray space in a rendered template is not a config error.
func splitAndTrim(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// err reports everything wrong at once, sorted so two runs read the same.
func (l *loader) err() error {
	if len(l.missing) == 0 && len(l.invalid) == 0 {
		return nil
	}

	var b strings.Builder
	b.WriteString("configuration is incomplete")

	if len(l.missing) > 0 {
		sort.Strings(l.missing)
		fmt.Fprintf(&b, "\n  missing (set in the Secrets Manager container, not here): %s",
			strings.Join(l.missing, ", "))
	}

	if len(l.invalid) > 0 {
		keys := make([]string, 0, len(l.invalid))
		for k := range l.invalid {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "\n  invalid %s: %s", k, l.invalid[k])
		}
	}

	return fmt.Errorf("%s", b.String())
}

// Common is what both services read. Everything here comes from the shared
// block at the top of service.env.tpl.j2.
type Common struct {
	// AppEnv is the environment name: beta, prod. Not a behaviour switch —
	// it is for logs and for telling two consoles apart.
	AppEnv string

	// AWSRegion is set because a container has no other source for it: no
	// profile, no shell rc, no instance metadata lookup on the startup path.
	AWSRegion string

	// NodeID identifies which app instance this is. With app_count = 1 it is
	// decoration; it stops being decoration the moment there are two.
	NodeID string

	// HealthListenAddr is a SEPARATE port from the service port, deliberately.
	// A health endpoint sharing a port with the ESL listener cannot tell "the
	// socket accepts" from "the service works", and only the second is worth
	// knowing.
	HealthListenAddr string
	HealthPath       string
}

func (l *loader) common() Common {
	return Common{
		AppEnv:           l.required("APP_ENV"),
		AWSRegion:        l.required("AWS_REGION"),
		NodeID:           l.optional("NODE_ID", "unknown"),
		HealthListenAddr: l.optional("HEALTH_LISTEN_ADDR", "0.0.0.0:8088"),
		HealthPath:       l.optional("HEALTH_PATH", "/healthz"),
	}
}

// Database is the Postgres connection, held by BOTH services.
//
// The controller has its own credential rather than reaching the database
// through the control-plane API. That is what lets a call be carried while the
// API is restarting — which it does on every deploy. See docs/API.md, "Why the
// split is where it is".
type Database struct {
	URL string

	// MaxConns is set explicitly and never left to the driver default.
	//
	// db.t4g.micro has 1 GiB of RAM, which is roughly 110 Postgres
	// connections TOTAL. Two services taking a generous default can consume
	// most of that between them, and the failure is `FATAL: sorry, too many
	// clients already` on whichever started second — a message that names
	// nothing about instance memory.
	MaxConns int
}

func (l *loader) database(defaultMaxConns int) Database {
	return Database{
		URL:      l.required("DATABASE_URL"),
		MaxConns: l.optionalInt("DATABASE_MAX_CONNS", defaultMaxConns),
	}
}
