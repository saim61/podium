package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/saim61/podium/internal/auth"
	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/leaderboard"
	"github.com/saim61/podium/internal/session"
)

type standingResponse struct {
	Scope      string              `json:"scope"`
	Period     leaderboard.Period  `json:"period"`
	Rank       int64               `json:"rank"`
	Points     int                 `json:"points"`
	Total      int64               `json:"total"`
	Neighbours []leaderboard.Entry `json:"neighbours"`
}

type leaderboardHandler struct {
	board    *leaderboard.Board
	sessions *session.Service
}

func (h *leaderboardHandler) handleGamePage(w http.ResponseWriter, r *http.Request) {
	slug := games.Slug(chi.URLParam(r, "game"))
	if _, err := h.sessions.Registry().Get(slug); err != nil {
		WriteError(w, r, NotFound("no such game"))
		return
	}
	h.writePage(w, r, leaderboard.Game(slug))
}

func (h *leaderboardHandler) handleGlobalPage(w http.ResponseWriter, r *http.Request) {
	h.writePage(w, r, leaderboard.Global())
}

func (h *leaderboardHandler) writePage(w http.ResponseWriter, r *http.Request, scope leaderboard.Scope) {
	period, err := leaderboard.ParsePeriod(r.URL.Query().Get("period"))
	if err != nil {
		WriteError(w, r, BadRequest(err.Error()).
			WithFields(map[string]string{"period": "must be all-time, daily, weekly or monthly"}))
		return
	}

	limit, err := intParam(r, "limit", 20, 1, leaderboard.MaxPageSize)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	offset, err := intParam(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	page, err := h.board.Page(r.Context(), scope, period, offset, limit)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, r, http.StatusOK, page)
}

func (h *leaderboardHandler) handleGameStanding(w http.ResponseWriter, r *http.Request) {
	slug := games.Slug(chi.URLParam(r, "game"))
	if _, err := h.sessions.Registry().Get(slug); err != nil {
		WriteError(w, r, NotFound("no such game"))
		return
	}
	h.writeStanding(w, r, leaderboard.Game(slug))
}

func (h *leaderboardHandler) handleGlobalStanding(w http.ResponseWriter, r *http.Request) {
	h.writeStanding(w, r, leaderboard.Global())
}

func (h *leaderboardHandler) writeStanding(w http.ResponseWriter, r *http.Request, scope leaderboard.Scope) {
	userID, ok := auth.UserIDFrom(r.Context())
	if !ok {
		WriteError(w, r, Unauthorized("authentication is required"))
		return
	}

	period, err := leaderboard.ParsePeriod(r.URL.Query().Get("period"))
	if err != nil {
		WriteError(w, r, BadRequest(err.Error()))
		return
	}

	neighbours, err := intParam(r, "neighbours", 2, 0, 25)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	standing, around, err := h.board.Standing(r.Context(), scope, period, userID, neighbours)
	if err != nil {
		if errors.Is(err, leaderboard.ErrUnranked) {
			// Not an error condition: a player who has not finished a game in this window
			// simply has no rank yet, and a 404 would make clients treat that as a failure.
			WriteJSON(w, r, http.StatusOK, standingResponse{
				Scope:      scope.Name(),
				Period:     period,
				Neighbours: []leaderboard.Entry{},
			})
			return
		}
		WriteError(w, r, err)
		return
	}

	total, err := h.board.Size(r.Context(), scope, period)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	if around == nil {
		around = []leaderboard.Entry{}
	}

	WriteJSON(w, r, http.StatusOK, standingResponse{
		Scope:      scope.Name(),
		Period:     period,
		Rank:       standing.Rank,
		Points:     standing.Points,
		Total:      total,
		Neighbours: around,
	})
}

func intParam(r *http.Request, name string, def, minimum, maximum int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, BadRequest(name + " must be a number").
			WithFields(map[string]string{name: "must be a number"})
	}
	if value < minimum || value > maximum {
		return 0, BadRequest(name + " is out of range").
			WithFields(map[string]string{
				name: "must be between " + strconv.Itoa(minimum) + " and " + strconv.Itoa(maximum),
			})
	}
	return value, nil
}
