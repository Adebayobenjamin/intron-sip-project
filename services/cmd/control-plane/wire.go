package main

import (
	"context"
	"net/http"
	"time"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/controlplane/auth"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/controlplane/handlers"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/controlplane/router"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/shared/config"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/shared/health"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/shared/httpserver"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/shared/ids"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/shared/logger"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/shared/run"
	"github.com/Adebayobenjamin/intron-sip-project/services/internal/store"
)

const (
	// stopWait is well under the unit's TimeoutStopSec of 60s for this
	// service. Requests in flight here are milliseconds.
	stopWait = 20 * time.Second

	// startupTimeout bounds migration and the first connection. Long enough
	// for a cold RDS instance to answer, short enough that systemd reports a
	// failed start rather than waiting indefinitely on a service that will
	// never come up.
	startupTimeout = 60 * time.Second
)

func runMain() error {
	cfg, err := config.LoadControlPlane()
	if err != nil {
		return err
	}

	log := logger.New(logger.Options{
		Service: "control-plane",
		Env:     cfg.AppEnv,
		NodeID:  cfg.NodeID,
		Level:   "info",
	})

	log.Info("starting",
		"api_addr", cfg.APIListenAddr,
		"health_addr", cfg.HealthListenAddr,
		"db_max_conns", cfg.MaxConns,
	)

	storeCfg := store.Config{
		URL:      cfg.URL,
		MaxConns: cfg.MaxConns,
		AppName:  "control-plane",
	}

	// Migrations run BEFORE the pool opens and before anything listens.
	//
	// This service is the only migrator. Failing here stops the service
	// starting, which is correct: a control plane serving configuration
	// against a schema it does not understand is worse than one that is down,
	// because only the second is obvious.
	startCtx, cancelStart := context.WithTimeout(context.Background(), startupTimeout)
	defer cancelStart()

	if err := store.Migrate(startCtx, storeCfg, log); err != nil {
		return err
	}

	pool, err := store.Open(startCtx, storeCfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	admissions := admission.New(pool, log, admission.Options{})

	checks := health.New()
	checks.Register("postgres", pool.HealthCheck)

	healthMux := http.NewServeMux()
	healthMux.HandleFunc(cfg.HealthPath, checks.Handler())

	// Every route is authenticated. CONTROL_PLANE_API_TOKEN works before any
	// key exists — including for the request that creates the first one.
	authenticator := auth.New(pool, cfg.APITokens)

	api := router.New(router.Deps{
		Auth: authenticator,
		Config: handlers.ConfigHandler{
			Store:     pool,
			Admission: admissions,
			Log:       log,
		},
		TrafficConfigs: handlers.TrafficConfigsHandler{
			Store:     pool,
			Admission: admissions,
			Log:       log,
		},
		Trunks: handlers.TrunksHandler{Store: pool, Admission: admissions, Log: log},
		Route:  handlers.Route{Admission: admissions, Log: log},
		Originate: handlers.OriginateHandler{
			Store: pool,
			UUID:  ids.Generator{},
			Log:   log,
		},
		Keys:  handlers.KeysHandler{Store: pool, Log: log},
		Calls: handlers.CallsHandler{Store: pool, Log: log},
		Log:   log,
	})

	app := run.New(log, stopWait)
	app.OnShutdown(func() { checks.SetDraining(true) })
	app.Add("health", httpserver.New("health", cfg.HealthListenAddr, healthMux, log).Run)
	app.Add("api", httpserver.New("api", cfg.APIListenAddr, api, log).Run)

	return app.Run()
}
