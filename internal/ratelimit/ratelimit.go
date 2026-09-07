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
	client *redis.Client
	script *goredis.Script
}

// Result describes one limit decision.
type Result struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

// New builds a limiter.
func New(client *redis.Client) (*Limiter, error) {
	source, err := scripts.ReadFile("lua/fixed_window.lua")
	if err != nil {
		return nil, fmt.Errorf("read fixed window script: %w", err)
	}

	return &Limiter{client: client, script: goredis.NewScript(string(source))}, nil
}

// Allow counts one event against key and reports whether it is permitted.
func (l *Limiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (Result, error) {
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
	if err := l.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("reset rate limit counter: %w", err)
	}
	return nil
}
