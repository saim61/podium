// Package testsupport starts real Postgres and Redis containers for integration tests.
//
// The leaderboard design lives in SQL constraints, sorted set semantics and Lua atomicity. A
// mocked store would assert that Podium calls the functions it calls, which is not the same as
// asserting the system works, so the tests talk to real servers.
package testsupport

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/platform/postgres"
	"github.com/saim61/podium/internal/platform/redis"
	"github.com/saim61/podium/internal/store/db"
	"github.com/saim61/podium/internal/store/migrations"
)

const (
	postgresImage = "postgres:18-alpine"
	redisImage    = "redis:8-alpine"
)

// One container per test binary, not per test. Starting Postgres costs seconds; isolating tests
// by truncating tables costs milliseconds.
var (
	pgOnce sync.Once
	pgDSN  string
	pgErr  error

	redisOnce sync.Once
	redisURL  string
	redisErr  error
)

func postgresDSN(ctx context.Context) (string, error) {
	pgOnce.Do(func() {
		container, err := tcpostgres.Run(ctx, postgresImage,
			tcpostgres.WithDatabase("podium"),
			tcpostgres.WithUsername("podium"),
			tcpostgres.WithPassword("podium"),
			tcpostgres.BasicWaitStrategies(),
			testcontainers.WithReuseByName("podium-test-postgres"),
		)
		if err != nil {
			pgErr = err
			return
		}

		dsn, err := container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			pgErr = err
			return
		}

		if _, err := migrations.Apply(ctx, dsn); err != nil {
			pgErr = err
			return
		}
		pgDSN = dsn
	})
	return pgDSN, pgErr
}

func redisConnURL(ctx context.Context) (string, error) {
	redisOnce.Do(func() {
		container, err := tcredis.Run(ctx, redisImage,
			testcontainers.WithReuseByName("podium-test-redis"),
		)
		if err != nil {
			redisErr = err
			return
		}

		url, err := container.ConnectionString(ctx)
		if err != nil {
			redisErr = err
			return
		}
		redisURL = url
	})
	return redisURL, redisErr
}

// PostgresDSN returns a connection string for a migrated database.
func PostgresDSN(t *testing.T) string {
	t.Helper()
	requireDocker(t)

	dsn, err := postgresDSN(t.Context())
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	return dsn
}

// Postgres returns a pool against a migrated, empty database. Tables are truncated before the
// test runs, so tests see a clean schema regardless of what ran before them.
func Postgres(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := PostgresDSN(t)

	pool, err := postgres.Open(t.Context(), config.Postgres{URL: dsn, MaxConns: 4, MinConns: 0})
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	truncateAll(t, pool)
	return pool
}

// Queries returns sqlc's query set bound to a clean database.
func Queries(t *testing.T) (*db.Queries, *pgxpool.Pool) {
	t.Helper()

	pool := Postgres(t)
	return db.New(pool), pool
}

// Redis returns a client against an empty database.
func Redis(t *testing.T) *redis.Client {
	t.Helper()
	requireDocker(t)

	url, err := redisConnURL(t.Context())
	if err != nil {
		t.Fatalf("start redis container: %v", err)
	}

	client, err := redis.Open(config.Redis{URL: url, PoolSize: 4})
	if err != nil {
		t.Fatalf("open redis: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := client.FlushDB(t.Context()).Err(); err != nil {
		t.Fatalf("flush redis: %v", err)
	}
	return client
}

func truncateAll(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	ctx := t.Context()

	rows, err := pool.Query(ctx, `
		SELECT tablename FROM pg_tables
		WHERE schemaname = 'public' AND tablename <> 'goose_db_version'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list tables: %v", err)
	}

	for _, table := range tables {
		// RESTART IDENTITY so generated ids start from 1 in every test, which keeps assertions
		// on ids stable no matter what order the suite runs in.
		if _, err := pool.Exec(ctx, "TRUNCATE TABLE "+table+" RESTART IDENTITY CASCADE"); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
}

func requireDocker(t *testing.T) {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping container-backed test in -short mode")
	}
}
