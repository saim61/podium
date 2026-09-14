package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/saim61/podium/internal/platform/observability"
)

// ServeMetrics runs a listener that serves nothing but /metrics and a liveness probe.
//
// For a process with no HTTP surface of its own - the worker - this is the only way a scraper
// can see it. It is deliberately separate from the API's listener so the two can be exposed
// differently: the API to the world, this one to the monitoring network only.
func ServeMetrics(ctx context.Context, addr string, log *slog.Logger, metrics *observability.Metrics) error {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}

	log.Info("metrics listener started", slog.String("addr", ln.Addr().String()))

	errs := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return srv.Close()
	}

	<-errs
	log.Info("metrics listener stopped")
	return nil
}
