// Package postgres builds Podium's connection pool.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/saim61/podium/internal/config"
)

// Open builds a connection pool. It does not connect: pgxpool dials lazily, and a process that
// refuses to start because a dependency is briefly unreachable crash-loops instead of coming up
// and reporting itself unready. Reachability is /readyz's job.
func Open(ctx context.Context, cfg config.Postgres) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("build connection pool: %w", err)
	}
	return pool, nil
}
