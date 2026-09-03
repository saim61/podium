// Package config loads Podium's runtime configuration from the environment and validates it
// before anything else starts.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Prefix is prepended to every environment variable Podium reads.
const Prefix = "PODIUM_"

// Env names the deployment environment.
type Env string

const (
	EnvDev  Env = "dev"
	EnvProd Env = "prod"
)

// Config is the fully resolved configuration for every Podium process.
type Config struct {
	Env      Env
	HTTP     HTTP
	Postgres Postgres
	Redis    Redis
	Log      Log
}

// HTTP configures the API listener.
type HTTP struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
}

// Postgres configures the connection pool.
type Postgres struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

// Redis configures the client.
type Redis struct {
	URL      string
	PoolSize int
}

// Log configures the structured logger.
type Log struct {
	Level  slog.Level
	Format Format
}

// Format selects a slog handler.
type Format string

const (
	FormatJSON Format = "json"
	FormatText Format = "text"
)

// Load reads configuration from the environment. It reports every problem it finds rather than
// only the first, so a misconfigured deployment needs one restart to diagnose instead of five.
func Load() (Config, error) {
	l := &loader{}

	cfg := Config{
		Env: Env(l.enum("ENV", string(EnvDev), string(EnvDev), string(EnvProd))),
		HTTP: HTTP{
			Addr:            l.str("HTTP_ADDR", ":8080"),
			ReadTimeout:     l.duration("HTTP_READ_TIMEOUT", 10*time.Second),
			WriteTimeout:    l.duration("HTTP_WRITE_TIMEOUT", 20*time.Second),
			IdleTimeout:     l.duration("HTTP_IDLE_TIMEOUT", 120*time.Second),
			ShutdownTimeout: l.duration("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second),
		},
		Postgres: Postgres{
			URL:             l.url("DATABASE_URL", "postgres://podium:podium@localhost:5432/podium?sslmode=disable"),
			MaxConns:        int32(l.intRange("POSTGRES_MAX_CONNS", 10, 1, 1000)),
			MinConns:        int32(l.intRange("POSTGRES_MIN_CONNS", 2, 0, 1000)),
			MaxConnLifetime: l.duration("POSTGRES_MAX_CONN_LIFETIME", time.Hour),
			MaxConnIdleTime: l.duration("POSTGRES_MAX_CONN_IDLE_TIME", 30*time.Minute),
		},
		Redis: Redis{
			URL:      l.url("REDIS_URL", "redis://localhost:6379/0"),
			PoolSize: l.intRange("REDIS_POOL_SIZE", 10, 1, 1000),
		},
		Log: Log{
			Level:  l.level("LOG_LEVEL", slog.LevelInfo),
			Format: Format(l.enum("LOG_FORMAT", string(FormatJSON), string(FormatJSON), string(FormatText))),
		},
	}

	if cfg.Postgres.MinConns > cfg.Postgres.MaxConns {
		l.fail("POSTGRES_MIN_CONNS", fmt.Sprintf("%d exceeds %sPOSTGRES_MAX_CONNS (%d)",
			cfg.Postgres.MinConns, Prefix, cfg.Postgres.MaxConns))
	}

	if err := l.err(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// IsProd reports whether this is a production deployment.
func (c Config) IsProd() bool { return c.Env == EnvProd }

type loader struct {
	errs []error
}

func (l *loader) fail(key, problem string) {
	l.errs = append(l.errs, fmt.Errorf("%s%s: %s", Prefix, key, problem))
}

func (l *loader) err() error {
	if len(l.errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration: %w", errors.Join(l.errs...))
}

func (l *loader) raw(key string) (string, bool) {
	v, ok := os.LookupEnv(Prefix + key)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	return v, true
}

func (l *loader) str(key, def string) string {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	return v
}

func (l *loader) url(key, def string) string {
	v := l.str(key, def)
	if _, err := url.Parse(v); err != nil {
		l.fail(key, "not a valid URL")
	}
	return v
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	switch {
	case err != nil:
		l.fail(key, fmt.Sprintf("%q is not a duration (try 30s, 5m, 1h)", v))
		return def
	case d <= 0:
		l.fail(key, "must be positive")
		return def
	}
	return d
}

func (l *loader) intRange(key string, def, minimum, maximum int) int {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	switch {
	case err != nil:
		l.fail(key, fmt.Sprintf("%q is not an integer", v))
		return def
	case n < minimum || n > maximum:
		l.fail(key, fmt.Sprintf("%d is outside %d..%d", n, minimum, maximum))
		return def
	}
	return n
}

func (l *loader) enum(key, def string, allowed ...string) string {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	v = strings.ToLower(v)
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	l.fail(key, fmt.Sprintf("%q is not one of %s", v, strings.Join(allowed, ", ")))
	return def
}

func (l *loader) level(key string, def slog.Level) slog.Level {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(v)); err != nil {
		l.fail(key, fmt.Sprintf("%q is not one of debug, info, warn, error", v))
		return def
	}
	return lvl
}
