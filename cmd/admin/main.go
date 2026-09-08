// Command admin performs operational tasks against a running Podium deployment.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/leaderboard"
	"github.com/saim61/podium/internal/platform/observability"
	"github.com/saim61/podium/internal/platform/postgres"
	"github.com/saim61/podium/internal/platform/redis"
	"github.com/saim61/podium/internal/scores"
	"github.com/saim61/podium/internal/user"
)

const usage = `podium admin

Usage:
  admin rebuild-leaderboards   Rebuild every leaderboard in Redis from Postgres
  admin status                 Report projection backlog and board sizes
`

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	if err := run(flag.Arg(0)); err != nil {
		fmt.Fprintf(os.Stderr, "admin: %v\n", err)
		os.Exit(1)
	}
}

func run(command string) error {
	if command == "" {
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("a command is required")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := observability.NewLogger(cfg.Log, os.Stdout)
	ctx := context.Background()

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

	switch command {
	case "rebuild-leaderboards":
		return rebuild(ctx, board, store, log)
	case "status":
		return status(ctx, board, store, log)
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", command)
	}
}

func rebuild(ctx context.Context, board *leaderboard.Board, store *scores.Store, log *slog.Logger) error {
	log.Info("rebuilding leaderboards from postgres")

	report, err := board.Rebuild(ctx, store)
	if err != nil {
		return err
	}

	log.Info("rebuild complete",
		slog.Int("keys", report.Keys),
		slog.Int("entries", report.Entries),
		slog.Int("periods", len(report.Periods)))
	return nil
}

func status(ctx context.Context, board *leaderboard.Board, store *scores.Store, log *slog.Logger) error {
	pending, err := store.PendingCount(ctx)
	if err != nil {
		return err
	}

	log.Info("projection backlog", slog.Int64("unprojected_scores", pending))

	for _, period := range leaderboard.Periods {
		size, err := board.Size(ctx, leaderboard.Global(), period)
		if err != nil {
			return err
		}
		log.Info("global board", slog.String("period", string(period)), slog.Int64("players", size))
	}
	return nil
}
