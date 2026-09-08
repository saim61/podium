package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/saim61/podium/internal/auth"
	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/leaderboard"
	"github.com/saim61/podium/internal/ratelimit"
	"github.com/saim61/podium/internal/session"
)

type gameResponse struct {
	Slug          games.Slug `json:"slug"`
	Name          string     `json:"name"`
	Summary       string     `json:"summary"`
	Rules         []string   `json:"rules"`
	Metric        string     `json:"metric"`
	LowerIsBetter bool       `json:"lower_is_better"`
	DurationSecs  int        `json:"duration_seconds,omitempty"`
	MaxPoints     int        `json:"max_points"`
}

func newGameResponse(d games.Definition) gameResponse {
	return gameResponse{
		Slug:          d.Slug,
		Name:          d.Name,
		Summary:       d.Summary,
		Rules:         d.Rules,
		Metric:        d.Metric,
		LowerIsBetter: d.LowerIsBetter,
		DurationSecs:  int(d.Duration.Seconds()),
		MaxPoints:     games.MaxPoints,
	}
}

type scoreResponse struct {
	Game       games.Slug `json:"game"`
	Metric     string     `json:"metric"`
	Raw        float64    `json:"raw"`
	Points     int        `json:"points"`
	AchievedAt time.Time  `json:"achieved_at"`
}

type placementResponse struct {
	Improved bool                                        `json:"improved"`
	Game     map[leaderboard.Period]leaderboard.Standing `json:"game"`
	Global   map[leaderboard.Period]leaderboard.Standing `json:"global"`
}

type sessionResponse struct {
	ID         uuid.UUID          `json:"id"`
	Game       games.Slug         `json:"game"`
	Status     string             `json:"status"`
	Moves      int                `json:"moves"`
	StartedAt  time.Time          `json:"started_at"`
	DeadlineAt *time.Time         `json:"deadline_at,omitempty"`
	State      json.RawMessage    `json:"state"`
	Score      *scoreResponse     `json:"score,omitempty"`
	Placement  *placementResponse `json:"placement,omitempty"`
}

// newSessionResponse renders a session. The "state" field carries the engine's View - the
// projection a client is allowed to see - never the private state that holds the answers.
func newSessionResponse(s session.Session) sessionResponse {
	response := sessionResponse{
		ID:         s.ID,
		Game:       s.Game,
		Status:     s.Status,
		Moves:      s.Moves,
		StartedAt:  s.StartedAt,
		DeadlineAt: s.DeadlineAt,
		State:      s.View,
	}

	if s.Score != nil {
		response.Score = &scoreResponse{
			Game:       s.Score.Game,
			Metric:     s.Score.Metric,
			Raw:        s.Score.Raw,
			Points:     s.Score.Points,
			AchievedAt: s.Score.AchievedAt,
		}
	}

	if s.Placement != nil {
		response.Placement = &placementResponse{
			Improved: s.Placement.Improved[leaderboard.AllTime],
			Game:     s.Placement.Game,
			Global:   s.Placement.Global,
		}
	}
	return response
}

type gamesHandler struct {
	sessions *session.Service
	limiter  *ratelimit.Limiter
	cfg      config.Auth
}

func (h *gamesHandler) handleListGames(w http.ResponseWriter, r *http.Request) {
	definitions := h.sessions.Registry().All()

	response := make([]gameResponse, 0, len(definitions))
	for _, d := range definitions {
		response = append(response, newGameResponse(d))
	}

	WriteJSON(w, r, http.StatusOK, map[string]any{"games": response})
}

func (h *gamesHandler) handleGetGame(w http.ResponseWriter, r *http.Request) {
	definition, err := h.sessions.Registry().Get(games.Slug(chi.URLParam(r, "slug")))
	if err != nil {
		WriteError(w, r, NotFound("no such game"))
		return
	}
	WriteJSON(w, r, http.StatusOK, newGameResponse(definition))
}

func (h *gamesHandler) handleStartSession(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFrom(r.Context())
	if !ok {
		WriteError(w, r, Unauthorized("authentication is required"))
		return
	}

	slug := games.Slug(chi.URLParam(r, "slug"))
	if _, err := h.sessions.Registry().Get(slug); err != nil {
		WriteError(w, r, NotFound("no such game"))
		return
	}

	if err := h.limitSessions(r, w, userID); err != nil {
		WriteError(w, r, err)
		return
	}

	created, err := h.sessions.Start(r.Context(), userID, slug)
	if err != nil {
		WriteError(w, r, sessionError(err))
		return
	}
	WriteJSON(w, r, http.StatusCreated, newSessionResponse(created))
}

func (h *gamesHandler) handleGetSession(w http.ResponseWriter, r *http.Request) {
	userID, id, err := sessionRequest(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	found, err := h.sessions.Get(r.Context(), userID, id)
	if err != nil {
		WriteError(w, r, sessionError(err))
		return
	}
	WriteJSON(w, r, http.StatusOK, newSessionResponse(found))
}

func (h *gamesHandler) handleMove(w http.ResponseWriter, r *http.Request) {
	userID, id, err := sessionRequest(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	var move json.RawMessage
	if err := DecodeJSON(r, &move); err != nil {
		WriteError(w, r, err)
		return
	}

	updated, err := h.sessions.Move(r.Context(), userID, id, move)
	if err != nil {
		WriteError(w, r, sessionError(err))
		return
	}
	WriteJSON(w, r, http.StatusOK, newSessionResponse(updated))
}

func (h *gamesHandler) handleFinish(w http.ResponseWriter, r *http.Request) {
	userID, id, err := sessionRequest(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	finished, err := h.sessions.Finish(r.Context(), userID, id)
	if err != nil {
		WriteError(w, r, sessionError(err))
		return
	}
	WriteJSON(w, r, http.StatusOK, newSessionResponse(finished))
}

func sessionRequest(r *http.Request) (int64, uuid.UUID, error) {
	userID, ok := auth.UserIDFrom(r.Context())
	if !ok {
		return 0, uuid.Nil, Unauthorized("authentication is required")
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		// A malformed id cannot name a session, and saying so plainly reveals nothing.
		return 0, uuid.Nil, NotFound("no such session")
	}
	return userID, id, nil
}

// sessionError maps domain failures onto status codes. Everything unrecognised falls through to
// a 500, which is the correct default for a failure nobody anticipated.
func sessionError(err error) error {
	switch {
	case errors.Is(err, session.ErrNotFound):
		return NotFound("no such session")

	case errors.Is(err, session.ErrFinished), errors.Is(err, games.ErrAlreadyDone):
		return Conflict("this session has already ended")

	case errors.Is(err, games.ErrDeadlinePassed):
		return Conflict("the time limit for this session has passed")

	case errors.Is(err, games.ErrInvalidMove):
		return BadRequest(err.Error())

	case errors.Is(err, games.ErrNoSuchGame):
		return NotFound("no such game")

	default:
		return err
	}
}

// sessionsPerWindow bounds how many sessions one account can open per login window. Starting a
// session is cheap for a client and costs the server a row and a seed, so it needs a ceiling.
const sessionsPerWindow = 60

func (h *gamesHandler) limitSessions(r *http.Request, w http.ResponseWriter, userID int64) error {
	key := "rl:sessions:user:" + strconv.FormatInt(userID, 10)

	return enforceLimit(r, w, h.limiter, key, sessionsPerWindow, h.cfg.LoginWindow)
}
