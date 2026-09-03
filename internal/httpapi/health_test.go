package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/saim61/podium/internal/config"
	"github.com/stretchr/testify/require"
)

func testRouter(t *testing.T, checks ...Check) http.Handler {
	t.Helper()

	cfg, err := config.Load()
	require.NoError(t, err)

	return NewRouter(Deps{Config: cfg, Logger: discardLogger(), Checks: checks})
}

func failing(name, msg string) Check {
	return Check{Name: name, Probe: func(context.Context) error { return errors.New(msg) }}
}

func passing(name string) Check {
	return Check{Name: name, Probe: func(context.Context) error { return nil }}
}

func TestLiveProbeIgnoresDependencies(t *testing.T) {
	rec := do(t, testRouter(t, failing("postgres", "connection refused")),
		http.MethodGet, "/healthz", "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"status":"ok"}`, rec.Body.String())
}

func TestReadyProbeWithNoChecks(t *testing.T) {
	rec := do(t, testRouter(t), http.MethodGet, "/readyz", "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"status":"ok"`)
}

func TestReadyProbeReportsEveryDependency(t *testing.T) {
	r := testRouter(t, passing("redis"),
		failing("postgres", "dial tcp 10.0.0.5:5432: connection refused"))

	rec := do(t, r, http.MethodGet, "/readyz", "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var got healthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, "unavailable", got.Status)
	require.Equal(t, "ok", got.Checks["redis"].Status)
	require.Equal(t, "error", got.Checks["postgres"].Status)
	require.Contains(t, got.Checks["postgres"].Error, "connection refused")
}

func TestReadyProbeWithholdsDetailInProduction(t *testing.T) {
	t.Setenv("PODIUM_ENV", "prod")

	r := testRouter(t, failing("postgres", "dial tcp 10.0.0.5:5432: connection refused"))

	rec := do(t, r, http.MethodGet, "/readyz", "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.NotContains(t, rec.Body.String(), "10.0.0.5")

	var got healthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, "error", got.Checks["postgres"].Status)
	require.Empty(t, got.Checks["postgres"].Error)
}
