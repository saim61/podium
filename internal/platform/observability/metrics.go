package observability

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is everything Podium exports to Prometheus.
//
// Built on its own registry rather than the package-global default. A global registry makes two
// instances in one process - which every integration test here builds - collide on registration,
// and it quietly pulls in whatever any dependency decided to register.
type Metrics struct {
	registry *prometheus.Registry

	HTTPRequests *prometheus.CounterVec
	HTTPDuration *prometheus.HistogramVec
	HTTPInFlight prometheus.Gauge

	SessionsStarted *prometheus.CounterVec
	ScoresRecorded  *prometheus.CounterVec
	ScorePoints     *prometheus.HistogramVec

	ProjectionPending prometheus.Gauge
	ProjectedTotal    prometheus.Counter
	RebuiltTotal      prometheus.Counter

	RealtimeConnections prometheus.Gauge
	RealtimeFrames      prometheus.Counter
	RealtimeDropped     prometheus.Counter
}

// NewMetrics builds the collectors and registers them.
func NewMetrics() *Metrics {
	registry := prometheus.NewRegistry()

	m := &Metrics{
		registry: registry,

		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "podium_http_requests_total",
			Help: "HTTP requests by route, method and status.",
		}, []string{"method", "route", "status"}),

		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "podium_http_request_duration_seconds",
			Help: "HTTP request duration by route and method.",
			// Tuned for this API: most reads are single-digit milliseconds, and the long tail
			// is reaction time's held response, which is deliberately seconds.
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}, []string{"method", "route"}),

		HTTPInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "podium_http_requests_in_flight",
			Help: "HTTP requests currently being served.",
		}),

		SessionsStarted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "podium_sessions_started_total",
			Help: "Game sessions opened, by game.",
		}, []string{"game"}),

		ScoresRecorded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "podium_scores_recorded_total",
			Help: "Scores written to the authoritative store, by game.",
		}, []string{"game"}),

		ScorePoints: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "podium_score_points",
			Help:    "Points earned per finished session, by game.",
			Buckets: prometheus.LinearBuckets(0, 1000, 11),
		}, []string{"game"}),

		ProjectionPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "podium_projection_pending",
			Help: "Scores recorded in Postgres but not yet published to Redis.",
		}),

		ProjectedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "podium_projected_total",
			Help: "Scores published to the leaderboards by the projector.",
		}),

		RebuiltTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "podium_leaderboards_rebuilt_total",
			Help: "Leaderboard keys rebuilt from Postgres.",
		}),

		RealtimeConnections: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "podium_realtime_connections",
			Help: "Open WebSocket connections on this instance.",
		}),

		RealtimeFrames: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "podium_realtime_frames_total",
			Help: "Snapshot frames delivered to subscribers.",
		}),

		RealtimeDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "podium_realtime_dropped_total",
			Help: "Subscribers dropped for failing to keep up.",
		}),
	}

	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.HTTPRequests, m.HTTPDuration, m.HTTPInFlight,
		m.SessionsStarted, m.ScoresRecorded, m.ScorePoints,
		m.ProjectionPending, m.ProjectedTotal, m.RebuiltTotal,
		m.RealtimeConnections, m.RealtimeFrames, m.RealtimeDropped,
	)
	return m
}

// Handler serves the metrics in Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		// A scrape that fails should say so in the response rather than in a log nobody reads.
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}

// Registry exposes the registry, for tests that want to read a value back.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }
