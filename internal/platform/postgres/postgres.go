// Package postgres builds Podium's connection pool.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/saim61/podium/internal/config"
)

// Defaults applied when a caller leaves a limit unset.
const (
	defaultMaxConns     = 10
	defaultConnLifetime = time.Hour
	defaultConnIdleTime = 30 * time.Minute
)

// positiveOr replaces a non-positive limit with a usable default.
//
// A zero MaxConnLifetime is the dangerous one. pgxpool computes a connection's expiry as
// createdAt.Add(MaxConnLifetime), so zero means every connection is born already expired: Acquire
// takes one, finds it expired, destroys it, and loops until it gives up with "too many failed
// attempts acquiring connection" - a message that points at hooks this code does not even use.
//
// It is worth guarding rather than documenting, because whether it bites depends on the clock.
// Windows' wall clock had not advanced between creation and the check in 100,000 out of 100,000
// samples, so the pool worked; Linux advanced every time, so nothing worked. That is a bug that
// passes locally and fails only in CI.
func positiveOr[T int32 | time.Duration](value, fallback T) T {
	if value <= 0 {
		return fallback
	}
	return value
}

// Open builds a connection pool. It does not connect: pgxpool dials lazily, and a process that
// refuses to start because a dependency is briefly unreachable crash-loops instead of coming up
// and reporting itself unready. Reachability is /readyz's job.
func Open(ctx context.Context, cfg config.Postgres) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	poolCfg.MaxConns = positiveOr(cfg.MaxConns, defaultMaxConns)
	poolCfg.MinConns = max(cfg.MinConns, 0)
	poolCfg.MaxConnLifetime = positiveOr(cfg.MaxConnLifetime, defaultConnLifetime)
	poolCfg.MaxConnIdleTime = positiveOr(cfg.MaxConnIdleTime, defaultConnIdleTime)

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("build connection pool: %w", err)
	}
	return pool, nil
}
