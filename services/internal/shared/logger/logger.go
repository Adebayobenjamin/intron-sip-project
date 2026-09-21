// Package logger builds the structured logger both services use.
//
// Output is JSON on stdout and nothing else. The systemd unit runs the
// container with --log-driver journald, and the CloudWatch agent ships the
// journal — so a log file on the box would be a second copy nobody rotates,
// on the root volume, next to the SSM agent that goes down with it when the
// disk fills.
package logger

import (
	"log/slog"
	"os"
	"strings"
)

// Options configures the logger. Service and NodeID are attached to every
// record because both containers log to the same journal on the same host,
// and separating them afterwards is otherwise guesswork.
type Options struct {
	Service string
	Env     string
	NodeID  string
	Level   string
}

// New returns a JSON logger writing to stdout.
func New(opts Options) *slog.Logger {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLevel(opts.Level),
	})

	return slog.New(h).With(
		slog.String("service", opts.Service),
		slog.String("env", opts.Env),
		slog.String("node", opts.NodeID),
	)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
