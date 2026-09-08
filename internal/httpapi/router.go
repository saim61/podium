package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/saim61/podium/internal/auth"
	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/leaderboard"
	"github.com/saim61/podium/internal/ratelimit"
	"github.com/saim61/podium/internal/session"
)

type Deps struct {
	Config      config.Config
	Logger      *slog.Logger
	Checks      []Check
	Auth        *auth.Service
	Sessions    *session.Service
	Leaderboard *leaderboard.Board
	Limiter     *ratelimit.Limiter
	Now         func() time.Time
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
	if d.Sessions != nil {
		mountGames(r, d)
	}
	if d.Leaderboard != nil && d.Sessions != nil {
		mountLeaderboards(r, d)
	}

	return r
}

func mountLeaderboards(r chi.Router, d Deps) {
	h := &leaderboardHandler{board: d.Leaderboard, sessions: d.Sessions}

	// Reading a leaderboard needs no account. It is the public face of the product, and
	// requiring a login to see who is winning would be an odd choice.
	r.Get("/v1/leaderboards/global", h.handleGlobalPage)
	r.Get("/v1/leaderboards/{game}", h.handleGamePage)

	if d.Auth == nil {
		return
	}

	// "Where am I" needs to know who is asking.
	r.Group(func(r chi.Router) {
		r.Use(Authenticate(d.Auth.Tokens()))

		r.Get("/v1/leaderboards/global/me", h.handleGlobalStanding)
		r.Get("/v1/leaderboards/{game}/me", h.handleGameStanding)
	})
}

func mountGames(r chi.Router, d Deps) {
	h := &gamesHandler{
		sessions: d.Sessions,
		limiter:  d.Limiter,
		cfg:      d.Config.Auth,
	}

	r.Get("/v1/games", h.handleListGames)
	r.Get("/v1/games/{slug}", h.handleGetGame)

	// Playing requires an account, because a score has to belong to somebody.
	if d.Auth == nil {
		return
	}

	r.Group(func(r chi.Router) {
		r.Use(Authenticate(d.Auth.Tokens()))

		r.Post("/v1/games/{slug}/sessions", h.handleStartSession)
		r.Get("/v1/sessions/{id}", h.handleGetSession)
		r.Post("/v1/sessions/{id}/moves", h.handleMove)
		r.Post("/v1/sessions/{id}/finish", h.handleFinish)
	})
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
