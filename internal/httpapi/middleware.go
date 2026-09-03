package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/saim61/podium/internal/platform/observability"
)

// RequestIDHeader carries the request id in both directions.
const RequestIDHeader = "X-Request-Id"

// DefaultMaxBodyBytes is the request body ceiling for JSON endpoints.
const DefaultMaxBodyBytes = 1 << 20

// RequestID assigns every request an id, reusing an inbound one so a trace started at a proxy or
// a client survives into Podium's logs.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" || len(id) > 64 {
			id = observability.NewRequestID()
		}

		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(observability.WithRequestID(r.Context(), id)))
	})
}

// Logger attaches a request-scoped logger and records one line per completed request.
func Logger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			reqLog := log.With(
				slog.String("request_id", observability.RequestID(ctx)),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)

			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()

			next.ServeHTTP(ww, r.WithContext(observability.WithLogger(ctx, reqLog)))

			reqLog.Info("request completed",
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
			)
		})
	}
}

// Recoverer converts a panic into a 500 so one bad handler cannot take the process down.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// A client that disconnects mid-write makes net/http panic with this sentinel. It is
			// not a bug and must not be logged as one.
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}

			observability.Logger(r.Context()).Error("panic recovered",
				slog.Any("panic", rec),
				slog.String("stack", string(debug.Stack())),
			)
			WriteError(w, r, Internal(fmt.Errorf("panic: %v", rec)))
		}()

		next.ServeHTTP(w, r)
	})
}

// MaxBody caps request body size. Applied once here rather than in every decode call, so no
// endpoint can forget it.
func MaxBody(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}
