// Command worker keeps Redis in step with Postgres and clears out data nothing can use.
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

	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/leaderboard"
	"github.com/saim61/podium/internal/platform/observability"
	"github.com/saim61/podium/internal/platform/postgres"
	"github.com/saim61/podium/internal/platform/redis"
	"github.com/saim61/podium/internal/projector"
	"github.com/saim61/podium/internal/reports"
	"github.com/saim61/podium/internal/scores"
	"github.com/saim61/podium/internal/user"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		os.Exit(1)
	}
}

// The worker is a separate process from the API on purpose. They scale on unrelated signals - the
// API on request rate, the worker on how much projection backlog there is - and a worker grinding
// through a backlog must not be able to add latency to a request.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := observability.NewLogger(cfg.Log, os.Stdout)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting podium worker", slog.String("env", string(cfg.Env)))

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

	board, err := leaderboard.New(rdb, user.NewDirectory(pool))
	if err != nil {
		return err
	}

	store := scores.NewStore(pool)
	reporter := reports.NewService(pool, board, games.NewRegistry())

	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		return projector.New(store, board, cfg.Worker, log).Run(groupCtx)
	})
	group.Go(func() error {
		return projector.NewHousekeeper(store, cfg.Worker, log, reporter).Run(groupCtx)
	})

	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	log.Info("worker stopped")
	return nil
}
