package httpapi

import (
	"net/http"
	"time"

	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/leaderboard"
	"github.com/saim61/podium/internal/reports"
)

type reportsHandler struct {
	service *reports.Service
}

// handleTopPlayers answers a period report.
//
// The window is chosen by a date rather than an offset like "yesterday", so a report is a stable
// thing that can be linked to and cached. ?date=2026-09-07 with period=weekly means the week
// containing that day.
func (h *reportsHandler) handleTopPlayers(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	period, err := leaderboard.ParsePeriod(query.Get("period"))
	if err != nil {
		WriteError(w, r, BadRequest(err.Error()).
			WithFields(map[string]string{"period": "must be all-time, daily, weekly or monthly"}))
		return
	}

	scope, err := h.scope(query.Get("game"))
	if err != nil {
		WriteError(w, r, err)
		return
	}

	at, err := reportDate(query.Get("date"))
	if err != nil {
		WriteError(w, r, err)
		return
	}

	limit, err := intParam(r, "limit", 10, 1, reports.MaxLimit)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	report, err := h.service.TopPlayers(r.Context(), reports.Request{
		Scope:  scope,
		Period: period,
		At:     at,
		Limit:  limit,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, r, http.StatusOK, report)
}

func (h *reportsHandler) scope(game string) (leaderboard.Scope, error) {
	if game == "" {
		return leaderboard.Global(), nil
	}

	if _, err := h.service.Games().Get(games.Slug(game)); err != nil {
		return leaderboard.Scope{}, NotFound("no such game")
	}
	return leaderboard.Game(games.Slug(game)), nil
}

// reportDate reads the day a report is about. Empty means now.
func reportDate(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}

	parsed, err := time.Parse(time.DateOnly, raw)
	if err != nil {
		return time.Time{}, BadRequest("date must look like 2026-09-07").
			WithFields(map[string]string{"date": "must be a date in YYYY-MM-DD form"})
	}

	// Midday, so the instant sits well inside the day whichever window it is resolved into and
	// no daylight-saving edge can push it into a neighbouring one.
	return parsed.UTC().Add(12 * time.Hour), nil
}
