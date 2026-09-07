package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/saim61/podium/internal/auth"
	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/ratelimit"
)

type Deps struct {
	Config  config.Config
	Logger  *slog.Logger
	Checks  []Check
	Auth    *auth.Service
	Limiter *ratelimit.Limiter
	Now     func() time.Time
}

func NewRouter(d Deps) http.Handler {
	if d.Now == nil {
		d.Now = time.Now
	}

	r := chi.NewRouter()

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

	if d.Auth != nil {
		mountAuth(r, d)
	}

	return r
}

func mountAuth(r chi.Router, d Deps) {
	h := &authHandler{
		service: d.Auth,
		limiter: d.Limiter,
		cfg:     d.Config.Auth,
		now:     d.Now,
	}

	r.Route("/v1/auth", func(r chi.Router) {
		r.Post("/register", h.handleRegister)
		r.Post("/login", h.handleLogin)
		r.Post("/refresh", h.handleRefresh)
		r.Post("/logout", h.handleLogout)
	})

	r.Group(func(r chi.Router) {
		r.Use(Authenticate(d.Auth.Tokens()))
		r.Get("/v1/me", h.handleMe)
	})
}
