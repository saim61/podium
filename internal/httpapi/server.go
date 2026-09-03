package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"

	"github.com/saim61/podium/internal/config"
)

// Serve runs the HTTP server until ctx is cancelled, then drains in-flight requests within the
// configured shutdown timeout.
//
// ready, when non-nil, is closed once the listener is accepting connections. Tests use it to
// avoid racing the server on startup.
func Serve(ctx context.Context, cfg config.HTTP, log *slog.Logger, h http.Handler, ready func(addr string)) error {
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:      h,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
		BaseContext:  func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}

	if ready != nil {
		ready(ln.Addr().String())
	}
	log.Info("http server listening", slog.String("addr", ln.Addr().String()))

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

	log.Info("shutting down", slog.String("grace", cfg.ShutdownTimeout.String()))

	// A fresh context: ctx is already cancelled, and passing it to Shutdown would abandon
	// in-flight requests immediately instead of draining them.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			log.Warn("shutdown deadline exceeded, closing remaining connections")
			return srv.Close()
		}
		return err
	}

	<-errs
	log.Info("shutdown complete")
	return nil
}
