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
	"github.com/saim61/podium/internal/realtime"
	"github.com/saim61/podium/internal/reports"
	"github.com/saim61/podium/internal/session"
)

type Deps struct {
	Config      config.Config
	Logger      *slog.Logger
	Checks      []Check
	Auth        *auth.Service
	Sessions    *session.Service
	Leaderboard *leaderboard.Board
	Realtime    *realtime.Server
	Tickets     *realtime.Tickets
	Reports     *reports.Service
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
	if d.Realtime != nil && d.Tickets != nil {
		mountRealtime(r, d)
	}
	if d.Reports != nil {
		mountReports(r, d)
	}

	return r
}

func mountReports(r chi.Router, d Deps) {
	h := &reportsHandler{service: d.Reports}

	r.Group(func(r chi.Router) {
		r.Use(RateLimit(d.Limiter, d.Config.Auth, "reads",
			d.Config.Auth.ReadsPerWindow, d.Config.Auth.RateWindow))

		r.Get("/v1/reports/top-players", h.handleTopPlayers)
	})
}

func mountRealtime(r chi.Router, d Deps) {
	h := &realtimeHandler{tickets: d.Tickets}

	// The handshake authenticates with the ticket in its query string, so this route must not
	// sit behind the bearer middleware - a browser cannot send a header with it.
	r.Get("/v1/ws", d.Realtime.Handler)

	if d.Auth == nil {
		return
	}

	r.Group(func(r chi.Router) {
		r.Use(Authenticate(d.Auth.Tokens()))
		r.Post("/v1/realtime/ticket", h.handleTicket)
	})
}

func mountLeaderboards(r chi.Router, d Deps) {
	h := &leaderboardHandler{board: d.Leaderboard, sessions: d.Sessions}

	// Reading a leaderboard needs no account. It is the public face of the product, and
	// requiring a login to see who is winning would be an odd choice - but an unauthenticated
	// read still needs a ceiling.
	r.Group(func(r chi.Router) {
		r.Use(RateLimit(d.Limiter, d.Config.Auth, "reads",
			d.Config.Auth.ReadsPerWindow, d.Config.Auth.RateWindow))

		r.Get("/v1/leaderboards/global", h.handleGlobalPage)
		r.Get("/v1/leaderboards/{game}", h.handleGamePage)
	})

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
		r.Post("/v1/sessions/{id}/finish", h.handleFinish)

		// Moves get their own, far higher ceiling: math-sprint is a race against 30 seconds
		// and a fast player legitimately sends dozens.
		r.Group(func(r chi.Router) {
			r.Use(RateLimit(d.Limiter, d.Config.Auth, "moves",
				d.Config.Auth.MovesPerWindow, d.Config.Auth.RateWindow))

			r.Post("/v1/sessions/{id}/moves", h.handleMove)
		})
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
