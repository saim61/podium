package httpapi

import (
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/platform/observability"
)

func metricsRouter(t *testing.T) (http.Handler, *observability.Metrics) {
	t.Helper()

	cfg, err := config.Load()
	require.NoError(t, err)

	metrics := observability.NewMetrics()

	return NewRouter(Deps{
		Config:  cfg,
		Logger:  discardLogger(),
		Metrics: metrics,
	}), metrics
}

// labelValues returns every distinct value a label takes on a metric, which is the number that
// decides whether a metrics store survives.
func labelValues(t *testing.T, metrics *observability.Metrics, metric, label string) []string {
	t.Helper()

	families, err := metrics.Registry().Gather()
	require.NoError(t, err)

	var values []string
	for _, family := range families {
		if family.GetName() != metric {
			continue
		}
		for _, m := range family.GetMetric() {
			for _, pair := range m.GetLabel() {
				if pair.GetName() == label {
					values = append(values, pair.GetValue())
				}
			}
		}
	}
	return values
}

func TestMetricsEndpointIsServed(t *testing.T) {
	router, _ := metricsRouter(t)

	// A labelled counter exports nothing until some combination of its labels has been
	// observed, so a scrape taken before any request would legitimately not mention it.
	do(t, router, http.MethodGet, "/healthz", "")

	rec := do(t, router, http.MethodGet, "/metrics", "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "podium_http_requests_total")
	require.Contains(t, rec.Body.String(), "go_goroutines", "the Go collector should be registered")
}

func TestRequestsAreCounted(t *testing.T) {
	router, metrics := metricsRouter(t)

	do(t, router, http.MethodGet, "/healthz", "")
	do(t, router, http.MethodGet, "/healthz", "")

	require.Equal(t, 2.0, counterValue(t, metrics, "podium_http_requests_total"))
}

func counterValue(t *testing.T, metrics *observability.Metrics, name string) float64 {
	t.Helper()

	families, err := metrics.Registry().Gather()
	require.NoError(t, err)

	total := 0.0
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, m := range family.GetMetric() {
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

// The label has to be the route pattern. Labelling by path would mint a series per session id -
// an endpoint with one shape becoming millions of series, which nothing downstream can absorb.
func TestRouteLabelIsThePatternNotThePath(t *testing.T) {
	router, metrics := metricsRouter(t)

	for _, path := range []string{
		"/v1/sessions/3f2504e0-4f89-11d3-9a0c-0305e82c3301",
		"/v1/sessions/8a1b3c4d-5e6f-7081-92a3-b4c5d6e7f809",
		"/v1/sessions/not-a-uuid",
	} {
		do(t, router, http.MethodGet, path, "")
	}

	routes := labelValues(t, metrics, "podium_http_requests_total", "route")
	require.NotEmpty(t, routes)

	for _, route := range routes {
		require.NotContains(t, route, "3f2504e0",
			"a session id must never appear in a metric label")
		require.NotContains(t, route, "8a1b3c4d")
	}
}

// Scanning for /wp-admin.php must not be able to add a series per probe.
func TestUnmatchedPathsShareOneLabel(t *testing.T) {
	router, metrics := metricsRouter(t)

	for _, path := range []string{"/wp-admin.php", "/.env", "/admin/config", "/etc/passwd"} {
		do(t, router, http.MethodGet, path, "")
	}

	routes := labelValues(t, metrics, "podium_http_requests_total", "route")

	require.Len(t, distinctValues(routes), 1)
	require.Equal(t, "unmatched", distinctValues(routes)[0])
}

func distinctValues(values []string) []string {
	seen := map[string]bool{}
	var out []string

	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func TestStatusIsLabelled(t *testing.T) {
	router, metrics := metricsRouter(t)

	do(t, router, http.MethodGet, "/healthz", "")
	do(t, router, http.MethodGet, "/v1/nope", "")

	statuses := distinctValues(labelValues(t, metrics, "podium_http_requests_total", "status"))

	require.ElementsMatch(t, []string{"200", "404"}, statuses)
}

func TestDurationsAreObserved(t *testing.T) {
	router, metrics := metricsRouter(t)

	do(t, router, http.MethodGet, "/healthz", "")

	families, err := metrics.Registry().Gather()
	require.NoError(t, err)

	var observed uint64
	for _, family := range families {
		if family.GetName() != "podium_http_request_duration_seconds" {
			continue
		}
		for _, m := range family.GetMetric() {
			observed += m.GetHistogram().GetSampleCount()
		}
	}
	require.Equal(t, uint64(1), observed)
}

func TestMetricsAreNotRegisteredGlobally(t *testing.T) {
	// Two instances in one process must not collide, which is what a global registry would do
	// and what every integration test here would hit.
	require.NotPanics(t, func() {
		observability.NewMetrics()
		observability.NewMetrics()
	})

	gathered, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	for _, family := range gathered {
		require.NotContains(t, family.GetName(), "podium_",
			"Podium metrics should live on their own registry")
	}
}
