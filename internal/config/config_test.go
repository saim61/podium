package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load()
	require.NoError(t, err)

	require.Equal(t, EnvDev, cfg.Env)
	require.False(t, cfg.IsProd())
	require.Equal(t, ":8080", cfg.HTTP.Addr)
	require.Equal(t, 15*time.Second, cfg.HTTP.ShutdownTimeout)
	require.Equal(t, slog.LevelInfo, cfg.Log.Level)
	require.Equal(t, FormatJSON, cfg.Log.Format)
	require.NotEmpty(t, cfg.Postgres.URL)
	require.NotEmpty(t, cfg.Redis.URL)
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("PODIUM_ENV", "prod")
	t.Setenv("PODIUM_JWT_SECRET", strings.Repeat("s", 32))
	t.Setenv("PODIUM_HTTP_ADDR", ":9000")
	t.Setenv("PODIUM_HTTP_READ_TIMEOUT", "45s")
	t.Setenv("PODIUM_POSTGRES_MAX_CONNS", "50")
	t.Setenv("PODIUM_LOG_LEVEL", "debug")
	t.Setenv("PODIUM_LOG_FORMAT", "TEXT")

	cfg, err := Load()
	require.NoError(t, err)

	require.True(t, cfg.IsProd())
	require.Equal(t, ":9000", cfg.HTTP.Addr)
	require.Equal(t, 45*time.Second, cfg.HTTP.ReadTimeout)
	require.Equal(t, int32(50), cfg.Postgres.MaxConns)
	require.Equal(t, slog.LevelDebug, cfg.Log.Level)
	require.Equal(t, FormatText, cfg.Log.Format)
}

func TestBlankValueFallsBackToDefault(t *testing.T) {
	t.Setenv("PODIUM_HTTP_ADDR", "   ")

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, ":8080", cfg.HTTP.Addr)
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	t.Setenv("PODIUM_ENV", "staging")
	t.Setenv("PODIUM_HTTP_READ_TIMEOUT", "soon")
	t.Setenv("PODIUM_HTTP_IDLE_TIMEOUT", "-5s")
	t.Setenv("PODIUM_POSTGRES_MAX_CONNS", "many")
	t.Setenv("PODIUM_REDIS_POOL_SIZE", "0")
	t.Setenv("PODIUM_LOG_LEVEL", "chatty")

	_, err := Load()
	require.Error(t, err)

	for _, want := range []string{
		"PODIUM_ENV",
		"PODIUM_HTTP_READ_TIMEOUT",
		"PODIUM_HTTP_IDLE_TIMEOUT",
		"PODIUM_POSTGRES_MAX_CONNS",
		"PODIUM_REDIS_POOL_SIZE",
		"PODIUM_LOG_LEVEL",
	} {
		require.ErrorContains(t, err, want)
	}
}

func TestAuthDefaults(t *testing.T) {
	cfg, err := Load()
	require.NoError(t, err)

	require.Equal(t, DevJWTSecret, cfg.Auth.JWTSecret)
	require.Equal(t, "podium", cfg.Auth.Issuer)
	require.Equal(t, 15*time.Minute, cfg.Auth.AccessTokenTTL)
	require.Equal(t, 720*time.Hour, cfg.Auth.RefreshTokenTTL)
	require.Equal(t, uint32(65536), cfg.Auth.Argon2.MemoryKiB)
	require.Equal(t, uint32(2), cfg.Auth.Argon2.Iterations)
	require.Equal(t, uint8(2), cfg.Auth.Argon2.Parallelism)
	require.False(t, cfg.Auth.TrustProxyIP)
}

func TestProductionRejectsDevelopmentJWTSecret(t *testing.T) {
	t.Setenv("PODIUM_ENV", "prod")

	_, err := Load()

	require.ErrorContains(t, err, "PODIUM_JWT_SECRET")
	require.ErrorContains(t, err, "development default")
}

func TestProductionRejectsShortJWTSecret(t *testing.T) {
	t.Setenv("PODIUM_ENV", "prod")
	t.Setenv("PODIUM_JWT_SECRET", "too-short")

	_, err := Load()

	require.ErrorContains(t, err, "at least 32 characters")
}

func TestDevelopmentAllowsDefaultJWTSecret(t *testing.T) {
	cfg, err := Load()

	require.NoError(t, err, "the development default must not block local work")
	require.Equal(t, DevJWTSecret, cfg.Auth.JWTSecret)
}

func TestProductionWarnsWhenProxyIPUntrusted(t *testing.T) {
	t.Setenv("PODIUM_ENV", "prod")
	t.Setenv("PODIUM_JWT_SECRET", strings.Repeat("s", 40))

	cfg, err := Load()
	require.NoError(t, err)

	require.Len(t, cfg.Warnings(), 1)
	require.Contains(t, cfg.Warnings()[0], "TRUST_PROXY_IP")
}

func TestProductionWithProxyIPTrustedHasNoWarnings(t *testing.T) {
	t.Setenv("PODIUM_ENV", "prod")
	t.Setenv("PODIUM_JWT_SECRET", strings.Repeat("s", 40))
	t.Setenv("PODIUM_TRUST_PROXY_IP", "true")

	cfg, err := Load()
	require.NoError(t, err)

	require.Empty(t, cfg.Warnings())
	require.True(t, cfg.Auth.TrustProxyIP)
}

func TestBooleanRejectsNonsense(t *testing.T) {
	t.Setenv("PODIUM_TRUST_PROXY_IP", "yes-please")

	_, err := Load()

	require.ErrorContains(t, err, "PODIUM_TRUST_PROXY_IP")
	require.ErrorContains(t, err, "not a boolean")
}

func TestArgon2CostIsBounded(t *testing.T) {
	t.Setenv("PODIUM_ARGON2_MEMORY_KIB", "16")

	_, err := Load()

	require.ErrorContains(t, err, "PODIUM_ARGON2_MEMORY_KIB")
}

func TestMinConnsCannotExceedMaxConns(t *testing.T) {
	t.Setenv("PODIUM_POSTGRES_MAX_CONNS", "4")
	t.Setenv("PODIUM_POSTGRES_MIN_CONNS", "9")

	_, err := Load()
	require.ErrorContains(t, err, "exceeds PODIUM_POSTGRES_MAX_CONNS")
}
