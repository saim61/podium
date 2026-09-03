package config

import (
	"log/slog"
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

func TestMinConnsCannotExceedMaxConns(t *testing.T) {
	t.Setenv("PODIUM_POSTGRES_MAX_CONNS", "4")
	t.Setenv("PODIUM_POSTGRES_MIN_CONNS", "9")

	_, err := Load()
	require.ErrorContains(t, err, "exceeds PODIUM_POSTGRES_MAX_CONNS")
}
