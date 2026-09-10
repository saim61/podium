// Command api serves Podium's HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sync/errgroup"

	"github.com/saim61/podium/internal/auth"
	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/httpapi"
	"github.com/saim61/podium/internal/leaderboard"
	"github.com/saim61/podium/internal/platform/observability"
	"github.com/saim61/podium/internal/platform/postgres"
	"github.com/saim61/podium/internal/platform/redis"
	"github.com/saim61/podium/internal/ratelimit"
	"github.com/saim61/podium/internal/realtime"
	"github.com/saim61/podium/internal/reports"
	"github.com/saim61/podium/internal/session"
	"github.com/saim61/podium/internal/user"
	"github.com/saim61/podium/internal/web"
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
	for _, warning := range cfg.Warnings() {
		log.Warn("configuration", slog.String("detail", warning))
	}

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

	authService, err := auth.NewService(pool, cfg.Auth)
	if err != nil {
		return err
	}

	limiter, err := ratelimit.New(rdb, cfg.Redis.OpTimeout)
	if err != nil {
		return err
	}

	registry := games.NewRegistry()

	board, err := leaderboard.New(rdb, user.NewDirectory(pool),
		leaderboard.WithAnnouncer(realtime.NewPublisher(rdb, cfg.Redis.OpTimeout)))
	if err != nil {
		return err
	}

	hub := realtime.NewHub(board, cfg.Realtime, log)
	bridge := realtime.NewBridge(rdb, hub, registry, log)
	tickets := realtime.NewTickets(rdb, cfg.Realtime, cfg.Redis.OpTimeout)

	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error { return hub.Run(groupCtx) })
	group.Go(func() error { return bridge.Run(groupCtx) })

	sessions := session.NewService(pool, registry,
		session.WithProjector(board),
		session.WithProjectTimeout(cfg.Redis.OpTimeout))

	demo, err := web.Handler()
	if err != nil {
		return err
	}

	router := httpapi.NewRouter(httpapi.Deps{
		Config:      cfg,
		Logger:      log,
		Auth:        authService,
		Sessions:    sessions,
		Leaderboard: board,
		Realtime:    realtime.NewServer(hub, tickets, registry, cfg.Realtime, log),
		Tickets:     tickets,
		Reports:     reports.NewService(pool, board, registry),
		Web:         demo,
		Limiter:     limiter,
		Checks: []httpapi.Check{
			{Name: "postgres", Probe: pool.Ping},
			{Name: "redis", Probe: redis.Ping(rdb)},
		},
	})

	group.Go(func() error { return httpapi.Serve(groupCtx, cfg.HTTP, log, router, nil) })

	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
