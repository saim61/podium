// Command migrate brings the database schema up to date and exits.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/platform/observability"
	"github.com/saim61/podium/internal/store/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := observability.NewLogger(cfg.Log, os.Stdout)

	applied, err := migrations.Apply(context.Background(), cfg.Postgres.URL)
	if err != nil {
		return err
	}

	for _, m := range applied {
		log.Info("migration applied", slog.Int64("version", m.Version), slog.String("name", m.Name))
	}

	version, err := migrations.Version(context.Background(), cfg.Postgres.URL)
	if err != nil {
		return err
	}

	log.Info("schema up to date",
		slog.Int64("version", version),
		slog.Int("applied", len(applied)),
	)
	return nil
}
