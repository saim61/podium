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
	Auth     Auth
	Worker   Worker
	Realtime Realtime

	warnings []string
}

// DevJWTSecret is the signing key used when none is configured. Load rejects it in production.
const DevJWTSecret = "podium-insecure-development-signing-key"

// Auth configures credentials, tokens and login throttling.
type Auth struct {
	JWTSecret       string
	Issuer          string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	Argon2          Argon2
	LoginMaxIP      int
	LoginMaxAccount int
	LoginWindow     time.Duration
	TrustProxyIP    bool
	ReadsPerWindow  int
	MovesPerWindow  int
	RateWindow      time.Duration
}

// Realtime configures the WebSocket fan-out.
type Realtime struct {
	FlushInterval   time.Duration
	TopN            int
	SendBuffer      int
	TicketTTL       time.Duration
	MaxChannels     int
	WriteTimeout    time.Duration
	PingInterval    time.Duration
	MaxConnsPerUser int
}

// Worker configures the background process that keeps Redis in step with Postgres.
type Worker struct {
	ProjectorInterval    time.Duration
	ProjectorBatch       int
	HousekeepingInterval time.Duration
	SessionMaxAge        time.Duration
	MetricsAddr          string
}

// Argon2 holds the password hashing cost. Values are recorded in every hash, so raising them
// later does not invalidate existing passwords.
type Argon2 struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
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
//
// The timeouts are deliberately tight. Redis holds only derived state here, so every caller is
// written to carry on without it - but "carry on" is worthless if each call first spends the
// default retry budget discovering Redis is gone. Failing in milliseconds is what makes
// degrading gracefully actually graceful.
type Redis struct {
	URL          string
	PoolSize     int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	MaxRetries   int
	OpTimeout    time.Duration
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
			URL:          l.url("REDIS_URL", "redis://localhost:6379/0"),
			PoolSize:     l.intRange("REDIS_POOL_SIZE", 10, 1, 1000),
			DialTimeout:  l.duration("REDIS_DIAL_TIMEOUT", 2*time.Second),
			ReadTimeout:  l.duration("REDIS_READ_TIMEOUT", time.Second),
			WriteTimeout: l.duration("REDIS_WRITE_TIMEOUT", time.Second),
			MaxRetries:   l.intRange("REDIS_MAX_RETRIES", 1, 0, 10),
			OpTimeout:    l.duration("REDIS_OP_TIMEOUT", 500*time.Millisecond),
		},
		Log: Log{
			Level:  l.level("LOG_LEVEL", slog.LevelInfo),
			Format: Format(l.enum("LOG_FORMAT", string(FormatJSON), string(FormatJSON), string(FormatText))),
		},
		Auth: Auth{
			JWTSecret:       l.str("JWT_SECRET", DevJWTSecret),
			Issuer:          l.str("JWT_ISSUER", "podium"),
			AccessTokenTTL:  l.duration("ACCESS_TOKEN_TTL", 15*time.Minute),
			RefreshTokenTTL: l.duration("REFRESH_TOKEN_TTL", 720*time.Hour),
			Argon2: Argon2{
				MemoryKiB:   uint32(l.intRange("ARGON2_MEMORY_KIB", 65536, 8192, 1048576)),
				Iterations:  uint32(l.intRange("ARGON2_ITERATIONS", 2, 1, 20)),
				Parallelism: uint8(l.intRange("ARGON2_PARALLELISM", 2, 1, 64)),
			},
			LoginMaxIP:      l.intRange("LOGIN_MAX_PER_IP", 20, 1, 10000),
			LoginMaxAccount: l.intRange("LOGIN_MAX_PER_ACCOUNT", 8, 1, 10000),
			LoginWindow:     l.duration("LOGIN_WINDOW", 15*time.Minute),
			TrustProxyIP:    l.boolean("TRUST_PROXY_IP", false),
			ReadsPerWindow:  l.intRange("READS_PER_WINDOW", 300, 1, 1_000_000),
			MovesPerWindow:  l.intRange("MOVES_PER_WINDOW", 600, 1, 1_000_000),
			RateWindow:      l.duration("RATE_WINDOW", time.Minute),
		},
		Realtime: Realtime{
			FlushInterval:   l.duration("RT_FLUSH_INTERVAL", 250*time.Millisecond),
			TopN:            l.intRange("RT_TOP_N", 10, 1, 100),
			SendBuffer:      l.intRange("RT_SEND_BUFFER", 16, 1, 1024),
			TicketTTL:       l.duration("RT_TICKET_TTL", 30*time.Second),
			MaxChannels:     l.intRange("RT_MAX_CHANNELS", 16, 1, 256),
			WriteTimeout:    l.duration("RT_WRITE_TIMEOUT", 5*time.Second),
			PingInterval:    l.duration("RT_PING_INTERVAL", 30*time.Second),
			MaxConnsPerUser: l.intRange("RT_MAX_CONNS_PER_USER", 4, 1, 64),
		},
		Worker: Worker{
			ProjectorInterval:    l.duration("PROJECTOR_INTERVAL", 5*time.Second),
			ProjectorBatch:       l.intRange("PROJECTOR_BATCH", 200, 1, 10000),
			HousekeepingInterval: l.duration("HOUSEKEEPING_INTERVAL", time.Hour),
			SessionMaxAge:        l.duration("SESSION_MAX_AGE", 2*time.Hour),
			MetricsAddr:          l.str("WORKER_METRICS_ADDR", ":9100"),
		},
	}

	if cfg.Postgres.MinConns > cfg.Postgres.MaxConns {
		l.fail("POSTGRES_MIN_CONNS", fmt.Sprintf("%d exceeds %sPOSTGRES_MAX_CONNS (%d)",
			cfg.Postgres.MinConns, Prefix, cfg.Postgres.MaxConns))
	}

	if cfg.Env == EnvProd {
		if cfg.Auth.JWTSecret == DevJWTSecret {
			l.fail("JWT_SECRET", "must be set in production, not left at the development default")
		}
		if len(cfg.Auth.JWTSecret) < minJWTSecretLength {
			l.fail("JWT_SECRET", fmt.Sprintf("must be at least %d characters", minJWTSecretLength))
		}
		if !cfg.Auth.TrustProxyIP {
			// Not fatal: a deployment may terminate TLS itself. Worth saying out loud, because
			// with this off behind a proxy every request appears to come from one address and
			// per-IP login throttling silently protects nothing.
			l.warn("TRUST_PROXY_IP is false in production; per-IP rate limits will see the " +
				"proxy address unless Podium is directly exposed")
		}
	}

	cfg.warnings = l.warns

	if err := l.err(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

const minJWTSecretLength = 32

// Warnings returns configuration that is legal but probably a mistake.
func (c Config) Warnings() []string { return c.warnings }

// IsProd reports whether this is a production deployment.
func (c Config) IsProd() bool { return c.Env == EnvProd }

type loader struct {
	errs  []error
	warns []string
}

func (l *loader) warn(message string) {
	l.warns = append(l.warns, message)
}

func (l *loader) boolean(key string, def bool) bool {
	v, ok := l.raw(key)
	if !ok {
		return def
	}

	parsed, err := strconv.ParseBool(v)
	if err != nil {
		l.fail(key, fmt.Sprintf("%q is not a boolean (try true or false)", v))
		return def
	}
	return parsed
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
