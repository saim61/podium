package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/saim61/podium/internal/config"
)

// Deps is everything the router needs to build Podium's HTTP surface.
type Deps struct {
	Config config.Config
	Logger *slog.Logger
	Checks []Check
}

// NewRouter assembles the middleware chain and routes.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	// Recoverer sits inside Logger so a panic is reported with the request-scoped logger and the
	// resulting 500 is still counted in the completion line.
	r.Use(RequestID)
	r.Use(Logger(d.Logger))
	r.Use(Recoverer)
	r.Use(MaxBody(DefaultMaxBodyBytes))

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, NotFound("no such endpoint"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, newError(http.StatusMethodNotAllowed, "method_not_allowed",
			"that method is not allowed on this endpoint"))
	})

	r.Get("/healthz", handleLive())
	r.Get("/readyz", handleReady(d.Checks, !d.Config.IsProd()))

	return r
}
