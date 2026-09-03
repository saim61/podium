package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/httpapi"
	"github.com/saim61/podium/internal/platform/postgres"
	"github.com/saim61/podium/internal/platform/redis"
	"github.com/saim61/podium/internal/testsupport"
)

func router(t *testing.T, checks ...httpapi.Check) http.Handler {
	t.Helper()

	cfg, err := config.Load()
	require.NoError(t, err)

	return httpapi.NewRouter(httpapi.Deps{
		Config: cfg,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Checks: checks,
	})
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

type readiness struct {
	Status string `json:"status"`
	Checks map[string]struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	} `json:"checks"`
}

func TestReadyReportsHealthyDependencies(t *testing.T) {
	pool := testsupport.Postgres(t)
	rdb := testsupport.Redis(t)

	h := router(t,
		httpapi.Check{Name: "postgres", Probe: pool.Ping},
		httpapi.Check{Name: "redis", Probe: redis.Ping(rdb)},
	)

	rec := get(t, h, "/readyz")
	require.Equal(t, http.StatusOK, rec.Code)

	var got readiness
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, "ok", got.Status)
	require.Equal(t, "ok", got.Checks["postgres"].Status)
	require.Equal(t, "ok", got.Checks["redis"].Status)
}

func TestReadyReportsUnreachablePostgres(t *testing.T) {
	rdb := testsupport.Redis(t)

	dead, err := postgres.Open(t.Context(), config.Postgres{
		URL:      "postgres://nobody@127.0.0.1:1/none?sslmode=disable",
		MaxConns: 1,
	})
	require.NoError(t, err, "Open must not dial, so a dead host is not an error here")
	t.Cleanup(dead.Close)

	h := router(t,
		httpapi.Check{Name: "postgres", Probe: dead.Ping},
		httpapi.Check{Name: "redis", Probe: redis.Ping(rdb)},
	)

	rec := get(t, h, "/readyz")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var got readiness
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, "unavailable", got.Status)
	require.Equal(t, "error", got.Checks["postgres"].Status)
	require.Equal(t, "ok", got.Checks["redis"].Status, "one dead dependency must not mask a live one")
}

func TestReadyReportsUnreachableRedis(t *testing.T) {
	pool := testsupport.Postgres(t)

	dead, err := redis.Open(config.Redis{URL: "redis://127.0.0.1:1/0", PoolSize: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = dead.Close() })

	h := router(t,
		httpapi.Check{Name: "postgres", Probe: pool.Ping},
		httpapi.Check{Name: "redis", Probe: redis.Ping(dead)},
	)

	rec := get(t, h, "/readyz")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestLiveIgnoresDeadDependencies(t *testing.T) {
	dead, err := postgres.Open(context.Background(), config.Postgres{
		URL:      "postgres://nobody@127.0.0.1:1/none?sslmode=disable",
		MaxConns: 1,
	})
	require.NoError(t, err)
	t.Cleanup(dead.Close)

	h := router(t, httpapi.Check{Name: "postgres", Probe: dead.Ping})

	rec := get(t, h, "/healthz")
	require.Equal(t, http.StatusOK, rec.Code, "liveness must never depend on a database")
}
