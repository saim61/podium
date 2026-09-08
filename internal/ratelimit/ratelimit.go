// Package ratelimit counts events in Redis so limits hold across every API instance.
package ratelimit

import (
	"context"
	"embed"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/saim61/podium/internal/platform/redis"
)

//go:embed lua/*.lua
var scripts embed.FS

// Limiter applies fixed-window limits.
//
// Fixed windows, not sliding: a caller can send up to 2x the limit across a window boundary. For
// login throttling that is an acceptable price for one counter and one round trip, and the point
// is to make credential stuffing expensive rather than to meter precisely.
type Limiter struct {
	client  *redis.Client
	script  *goredis.Script
	timeout time.Duration
}

// Result describes one limit decision.
type Result struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

// New builds a limiter. timeout bounds any single Redis call; zero picks a sane default.
//
// The bound has to be here rather than only on the client, because go-redis multiplies its own
// retries: the pool retries a failed dial and the command layer retries the pool, so a dead
// Redis costs dial timeout x pool attempts x command attempts per call. Measured against a
// stopped Redis that came to 83 seconds for a single login. A deadline the caller owns is the
// only thing that actually bounds it.
func New(client *redis.Client, timeout time.Duration) (*Limiter, error) {
	source, err := scripts.ReadFile("lua/fixed_window.lua")
	if err != nil {
		return nil, fmt.Errorf("read fixed window script: %w", err)
	}

	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}

	return &Limiter{
		client:  client,
		script:  goredis.NewScript(string(source)),
		timeout: timeout,
	}, nil
}

// Allow counts one event against key and reports whether it is permitted.
func (l *Limiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	raw, err := l.script.Run(ctx, l.client, []string{key}, limit, window.Milliseconds()).Slice()
	if err != nil {
		return Result{}, fmt.Errorf("run rate limit script: %w", err)
	}
	if len(raw) != 3 {
		return Result{}, fmt.Errorf("rate limit script returned %d values, want 3", len(raw))
	}

	allowed, ok := raw[0].(int64)
	if !ok {
		return Result{}, fmt.Errorf("rate limit script returned %T for allowed", raw[0])
	}
	remaining, ok := raw[1].(int64)
	if !ok {
		return Result{}, fmt.Errorf("rate limit script returned %T for remaining", raw[1])
	}
	ttlMillis, ok := raw[2].(int64)
	if !ok {
		return Result{}, fmt.Errorf("rate limit script returned %T for ttl", raw[2])
	}

	return Result{
		Allowed:    allowed == 1,
		Remaining:  int(remaining),
		RetryAfter: time.Duration(ttlMillis) * time.Millisecond,
	}, nil
}

// Reset clears a counter, so a successful login can forgive earlier failures.
func (l *Limiter) Reset(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	if err := l.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("reset rate limit counter: %w", err)
	}
	return nil
}
