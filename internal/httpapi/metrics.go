package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"github.com/saim61/podium/internal/platform/observability"
)

// Measure records request counts and durations.
//
// The route label is chi's *pattern*, not the path. Labelling by path would mint a new time
// series for every session id and every game slug a client invents - millions of series for an
// endpoint that has one shape. Prometheus has no defence against that; the caller has to not do
// it.
//
// The pattern is only known once chi has matched, so it is read after the handler returns.
func Measure(metrics *observability.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			metrics.HTTPInFlight.Inc()
			defer metrics.HTTPInFlight.Dec()

			ww := chimiddleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()

			next.ServeHTTP(ww, r)

			route := routePattern(r)
			metrics.HTTPDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
			metrics.HTTPRequests.WithLabelValues(r.Method, route, strconv.Itoa(ww.Status())).Inc()
		})
	}
}

// routePattern is the matched route, or a fixed placeholder when nothing matched.
//
// Unmatched requests all share one label. Without that, anyone scanning for /wp-admin.php could
// add a series per probe - a cheap way to fill a metrics store from outside.
func routePattern(r *http.Request) string {
	ctx := chi.RouteContext(r.Context())
	if ctx == nil {
		return "unmatched"
	}

	pattern := ctx.RoutePattern()
	if pattern == "" {
		return "unmatched"
	}
	return pattern
}
