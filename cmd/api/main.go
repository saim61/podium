// Command api serves Podium's HTTP API.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/httpapi"
	"github.com/saim61/podium/internal/platform/observability"
	"github.com/saim61/podium/internal/platform/postgres"
	"github.com/saim61/podium/internal/platform/redis"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "api: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := observability.NewLogger(cfg.Log, os.Stdout)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting podium api", slog.String("env", string(cfg.Env)))

	pool, err := postgres.Open(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pool.Close()

	rdb, err := redis.Open(cfg.Redis)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()

	router := httpapi.NewRouter(httpapi.Deps{
		Config: cfg,
		Logger: log,
		Checks: []httpapi.Check{
			{Name: "postgres", Probe: pool.Ping},
			{Name: "redis", Probe: redis.Ping(rdb)},
		},
	})

	return httpapi.Serve(ctx, cfg.HTTP, log, router, nil)
}
