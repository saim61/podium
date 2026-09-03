// Package redis builds Podium's Redis client.
package redis

import (
	"context"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
	"github.com/saim61/podium/internal/config"
)

// Client is the Redis handle used across Podium.
type Client = goredis.Client

// Open builds a Redis client. Like the Postgres pool it connects lazily, leaving reachability to
// /readyz rather than to process startup.
func Open(cfg config.Redis) (*Client, error) {
	opts, err := goredis.ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}

	opts.PoolSize = cfg.PoolSize

	return goredis.NewClient(opts), nil
}

// Ping reports whether Redis is reachable.
func Ping(client *Client) func(context.Context) error {
	return func(ctx context.Context) error {
		return client.Ping(ctx).Err()
	}
}
