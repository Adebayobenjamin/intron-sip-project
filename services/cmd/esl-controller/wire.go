package main

import (
	"context"
	"net/http"
	"time"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/ctrl"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/eslclient"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/shared/config"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/shared/health"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/shared/httpserver"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/shared/logger"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/shared/run"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/store"
)

const (
	// stopWait sits under the unit's TimeoutStopSec of 120s, leaving room for
	// Docker and systemd to do their own work before the deadline. This
	// service holds a socket per call, so the window is real: it is how long
	// in-flight calls have to end cleanly instead of mid-write.
	stopWait = 100 * time.Second

	// startupTimeout bounds the first connection and the schema check.
	startupTimeout = 60 * time.Second

	// warmLimit caps how many DIDs are preloaded. A guard rather than a
	// policy: a warm that quietly loads a million rows is a slow startup
	// nobody attributes to this. Ranges are not enumerated — one can cover
	// ten thousand numbers, and materialising them would evict everything
	// else to hold numbers nobody has called.
	warmLimit = 5000
)

func runMain() error {
	cfg, err := config.LoadEslController()
	if err != nil {
		return err
	}

	log := logger.New(logger.Options{
		Service: "esl-controller",
		Env:     cfg.AppEnv,
		NodeID:  cfg.NodeID,
		Level:   "info",
	})

	log.Info("starting",
		"esl_addr", cfg.ListenAddr,
		"health_addr", cfg.HealthListenAddr,
		"keepalive", cfg.TCPKeepalive.String(),
		"freeswitch_nodes", cfg.FreeswitchNodes,
		"db_max_conns", cfg.MaxConns,
	)

	// Empty LiveKit config is the intended state until its address is
	// confirmed, but it should be visible in the log rather than discovered
	// when the first call fails to reach an agent.
	if !cfg.LiveKitConfigured() {
		log.Warn("livekit not configured; the AI leg will not connect")
	}

	startCtx, cancelStart := context.WithTimeout(context.Background(), startupTimeout)
	defer cancelStart()

	pool, err := store.Open(startCtx, store.Config{
		URL:      cfg.URL,
		MaxConns: cfg.MaxConns,
		AppName:  "esl-controller",
	})
	if err != nil {
		return err
	}
	defer pool.Close()

	// This service NEVER migrates — control-plane owns the schema. It only
	// checks that the schema it was built against is present, and refuses to
	// start if it is not. The alternative is a controller that starts, queries
	// a column that does not exist, and fails one call at a time, which reads
	// as an intermittent fault rather than a deploy done in the wrong order.
	if err := pool.AssertSchema(startCtx); err != nil {
		return err
	}

	admissions := admission.New(pool, log, admission.Options{})

	// Warm before anything can accept a call.
	//
	// A cold process after a deploy otherwise takes its first calls at
	// database latency — and fails all of them if the database is the thing
	// that is wrong, with no stale entry to fall back on. That is the worst
	// possible moment for the cache to be empty.
	//
	// A failure here is NOT fatal. Warming is an optimisation; the service can
	// resolve on demand. Refusing to start over it would turn a slow database
	// into an outage, which is the opposite of what the cache is for.
	if routes, err := pool.ActiveRoutes(startCtx, warmLimit); err != nil {
		log.Warn("could not warm the admission cache; resolving on demand", "error", err)
	} else {
		admissions.Warm(startCtx, routes)
	}

	// "Is the cache warm?" is the first question when call setup is slow, and
	// it should not need a profiler to answer.
	log.Info("admission ready", "cached_routes", admissions.CacheSize())

	// Inbound ESL: this service connecting OUT to each FreeSWITCH node, for
	// originate, and for asking about a channel whose own socket may be gone.
	// The opposite direction from the listener below, and the two fail
	// differently.
	switches := eslclient.New(eslclient.Config{
		Nodes:     cfg.FreeswitchNodes,
		Port:      cfg.FreeswitchESLPort,
		Password:  cfg.FreeswitchESLPassword,
		Keepalive: cfg.TCPKeepalive,
	}, log)

	// switches is passed in as the channel probe: after a bridge ends with no
	// hangup event, the controller asks FreeSWITCH whether the channel is
	// still up — which is how a TRANSFER is told from a call that finished.
	listener := ctrl.New(ctrl.Config{
		Addr:          cfg.ListenAddr,
		Keepalive:     cfg.TCPKeepalive,
		Node:          cfg.NodeID,
		RecordingPath: cfg.RecordingPath,
	}, admissions, pool, pool, switches, pool, log)

	checks := health.New()
	checks.Register("postgres", pool.HealthCheck)
	checks.Register("freeswitch-esl", switches.HealthCheck)

	healthMux := http.NewServeMux()
	healthMux.HandleFunc(cfg.HealthPath, checks.Handler())

	app := run.New(log, stopWait)

	// Flip health first so anything watching sees the shutdown before calls
	// start ending, rather than after.
	app.OnShutdown(func() {
		checks.SetDraining(true)
		if n := listener.Active(); n > 0 {
			log.Warn("stopping with calls in progress", "calls", n)
		}
	})

	app.Add("health", httpserver.New("health", cfg.HealthListenAddr, healthMux, log).Run)
	app.Add("esl", listener.Run)
	app.Add("esl-pool", switches.Run)

	// Reclaims slots held by a controller that died without releasing them.
	// Without it one crash permanently reduces a tenant's capacity, and the
	// symptom is congestion on a platform carrying no calls.
	app.Add("sweeper", ctrl.NewSweeper(pool, log).Run)

	// Dials the calls the control plane queues. It cannot dial from there:
	// only this service holds ESL connections, so the handoff is a row claimed
	// with FOR UPDATE SKIP LOCKED.
	app.Add("originate", ctrl.NewOriginator(
		pool, switches, cfg.NodeID, cfg.CallbackTarget, cfg.EdgeSIPRoute, log).Run)

	return app.Run()
}
