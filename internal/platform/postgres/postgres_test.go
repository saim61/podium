package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/saim61/podium/internal/config"
)

const testDSN = "postgres://podium:podium@127.0.0.1:5432/podium?sslmode=disable"

// Open does not dial, so these assert configuration without needing a database.

func TestOpenAppliesConfiguredLimits(t *testing.T) {
	pool, err := Open(context.Background(), config.Postgres{
		URL:             testDSN,
		MaxConns:        7,
		MinConns:        2,
		MaxConnLifetime: 90 * time.Minute,
		MaxConnIdleTime: 4 * time.Minute,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	cfg := pool.Config()
	require.Equal(t, int32(7), cfg.MaxConns)
	require.Equal(t, int32(2), cfg.MinConns)
	require.Equal(t, 90*time.Minute, cfg.MaxConnLifetime)
	require.Equal(t, 4*time.Minute, cfg.MaxConnIdleTime)
}

// The regression this exists for.
//
// A zero MaxConnLifetime makes pgxpool treat every connection as expired the instant it is
// created, so Acquire destroys each one and eventually fails with "too many failed attempts
// acquiring connection". Whether it bites depends on clock resolution, so it passed on Windows
// and failed every time on Linux - the worst kind of bug to leave reachable.
func TestZeroLimitsNeverProduceASelfDestroyingPool(t *testing.T) {
	pool, err := Open(context.Background(), config.Postgres{URL: testDSN})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	cfg := pool.Config()
	require.Positive(t, cfg.MaxConnLifetime,
		"a zero lifetime expires every connection at birth")
	require.Positive(t, cfg.MaxConnIdleTime)
	require.Positive(t, cfg.MaxConns)
}

func TestNegativeLimitsAreAlsoCorrected(t *testing.T) {
	pool, err := Open(context.Background(), config.Postgres{
		URL:             testDSN,
		MaxConns:        -1,
		MinConns:        -5,
		MaxConnLifetime: -time.Hour,
		MaxConnIdleTime: -time.Minute,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	cfg := pool.Config()
	require.Positive(t, cfg.MaxConns)
	require.GreaterOrEqual(t, cfg.MinConns, int32(0))
	require.Positive(t, cfg.MaxConnLifetime)
	require.Positive(t, cfg.MaxConnIdleTime)
}

// The clock difference that hid the bug, asserted so the reasoning is not just a comment.
func TestAZeroLifetimeWouldExpireConnectionsImmediately(t *testing.T) {
	created := time.Now()
	expiry := created.Add(0)

	require.True(t, time.Now().After(expiry) || time.Now().Equal(expiry),
		"with a zero lifetime a connection is expired the moment it is checked")
}

func TestOpenRejectsAnUnparsableURL(t *testing.T) {
	_, err := Open(context.Background(), config.Postgres{URL: "://not a url"})

	require.Error(t, err)
	require.Contains(t, err.Error(), "parse database url")
}

// Open must not dial, so an unreachable host is not an error here - readiness reports that.
func TestOpenDoesNotConnect(t *testing.T) {
	pool, err := Open(context.Background(), config.Postgres{
		URL: "postgres://nobody@127.0.0.1:1/none?sslmode=disable",
	})

	require.NoError(t, err)
	t.Cleanup(pool.Close)
}
