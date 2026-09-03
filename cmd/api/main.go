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

	router := httpapi.NewRouter(httpapi.Deps{Config: cfg, Logger: log})

	return httpapi.Serve(ctx, cfg.HTTP, log, router, nil)
}
