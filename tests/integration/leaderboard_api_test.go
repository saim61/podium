package integration

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/leaderboard"
)

type pageBody struct {
	Scope   string `json:"scope"`
	Period  string `json:"period"`
	Total   int64  `json:"total"`
	Offset  int    `json:"offset"`
	Entries []struct {
		Rank     int64  `json:"rank"`
		Username string `json:"username"`
		Points   int    `json:"points"`
	} `json:"entries"`
}

type standingBody struct {
	Scope      string `json:"scope"`
	Period     string `json:"period"`
	Rank       int64  `json:"rank"`
	Points     int    `json:"points"`
	Total      int64  `json:"total"`
	Neighbours []struct {
		Rank     int64  `json:"rank"`
		Username string `json:"username"`
		Points   int    `json:"points"`
	} `json:"neighbours"`
}

// playMemory finishes a memory session at a chosen level, which is a predictable way to put a
// known score on the board through the real API.
func (h *authHarness) playMemory(t *testing.T, token string, levels int) sessionBody {
	t.Helper()

	opened := h.startSession(t, token, games.Memory)

	current := opened
	for range levels {
		var view struct {
			Sequence []int `json:"sequence"`
		}
		require.NoError(t, json.Unmarshal(current.State, &view))
		current = h.move(t, token, opened.ID, map[string][]int{"answer": view.Sequence})
	}
	return h.finish(t, token, opened.ID)
}

func TestFinishingAGamePutsYouOnTheLeaderboard(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")

	finished := h.playMemory(t, player.Tokens.AccessToken, 4)
	require.NotNil(t, finished.Score)

	rec := h.do(t, http.MethodGet, "/v1/leaderboards/memory", nil, "")
	require.Equal(t, http.StatusOK, rec.Code)

	page := decodeInto[pageBody](t, rec)
	require.Equal(t, "memory", page.Scope)
	require.Equal(t, "all-time", page.Period)
	require.Equal(t, int64(1), page.Total)
	require.Len(t, page.Entries, 1)
	require.Equal(t, "saeem", page.Entries[0].Username)
	require.Equal(t, int64(1), page.Entries[0].Rank)
	require.Equal(t, finished.Score.Points, page.Entries[0].Points)
}

// The score has to be durably recorded and marked projected, or phase 5's sweeper would keep
// re-publishing it forever.
func TestProjectionIsRecordedAgainstTheScoreEvent(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")

	h.playMemory(t, player.Tokens.AccessToken, 3)

	var unprojected int
	require.NoError(t, h.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM score_events WHERE projected_at IS NULL").Scan(&unprojected))

	require.Zero(t, unprojected, "a successfully projected score should be stamped")
}

func TestFinishResponseCarriesTheNewRanks(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")

	opened := h.startSession(t, player.Tokens.AccessToken, games.Memory)
	rec := h.do(t, http.MethodPost, "/v1/sessions/"+opened.ID.String()+"/finish", nil,
		player.Tokens.AccessToken)
	require.Equal(t, http.StatusOK, rec.Code)

	body := decodeInto[struct {
		Placement *struct {
			Improved bool                            `json:"improved"`
			Game     map[string]leaderboard.Standing `json:"game"`
			Global   map[string]leaderboard.Standing `json:"global"`
		} `json:"placement"`
	}](t, rec)

	require.NotNil(t, body.Placement, "finishing should report where the score landed")
	require.True(t, body.Placement.Improved)
	require.Equal(t, int64(1), body.Placement.Game["all-time"].Rank)
	require.Equal(t, int64(1), body.Placement.Global["all-time"].Rank)
	require.Contains(t, body.Placement.Game, "daily")
	require.Contains(t, body.Placement.Game, "weekly")
	require.Contains(t, body.Placement.Game, "monthly")
}

func TestLeaderboardOrdersPlayersByScore(t *testing.T) {
	h := newAuthHarness(t)

	strong := h.register(t, "strong")
	weak := h.register(t, "weak")

	h.playMemory(t, strong.Tokens.AccessToken, 6)
	h.playMemory(t, weak.Tokens.AccessToken, 2)

	page := decodeInto[pageBody](t,
		h.do(t, http.MethodGet, "/v1/leaderboards/memory", nil, ""))

	require.Len(t, page.Entries, 2)
	require.Equal(t, "strong", page.Entries[0].Username)
	require.Equal(t, "weak", page.Entries[1].Username)
	require.Greater(t, page.Entries[0].Points, page.Entries[1].Points)
}

func TestGlobalLeaderboardSumsAcrossGames(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	token := player.Tokens.AccessToken

	memory := h.playMemory(t, token, 5)

	sprint := h.startSession(t, token, games.MathSprint)
	var view struct {
		Question string `json:"question"`
	}
	require.NoError(t, json.Unmarshal(sprint.State, &view))
	h.move(t, token, sprint.ID, map[string]int{"answer": solve(t, view.Question)})
	sprintScore := h.finish(t, token, sprint.ID)

	page := decodeInto[pageBody](t,
		h.do(t, http.MethodGet, "/v1/leaderboards/global", nil, ""))

	require.Len(t, page.Entries, 1)
	require.Equal(t, memory.Score.Points+sprintScore.Score.Points, page.Entries[0].Points,
		"the global score is the sum of per-game bests")
}

func TestLeaderboardIsReadableWithoutAnAccount(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	h.playMemory(t, player.Tokens.AccessToken, 3)

	for _, path := range []string{"/v1/leaderboards/memory", "/v1/leaderboards/global"} {
		rec := h.do(t, http.MethodGet, path, nil, "")
		require.Equal(t, http.StatusOK, rec.Code, path)
	}
}

func TestMyStandingNeedsAuthentication(t *testing.T) {
	h := newAuthHarness(t)

	rec := h.do(t, http.MethodGet, "/v1/leaderboards/memory/me", nil, "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestMyStandingReportsRankAndNeighbours(t *testing.T) {
	h := newAuthHarness(t)

	for i, name := range []string{"first", "second", "third"} {
		player := h.register(t, name)
		h.playMemory(t, player.Tokens.AccessToken, 6-i*2)
	}

	me := h.register(t, "myself")
	h.playMemory(t, me.Tokens.AccessToken, 3)

	rec := h.do(t, http.MethodGet, "/v1/leaderboards/memory/me", nil, me.Tokens.AccessToken)
	require.Equal(t, http.StatusOK, rec.Code)

	standing := decodeInto[standingBody](t, rec)
	require.Equal(t, "memory", standing.Scope)
	require.Positive(t, standing.Rank)
	require.Equal(t, int64(4), standing.Total)
	require.NotEmpty(t, standing.Neighbours)
}

// A player who has not finished a game in this window is unranked, which is a normal state and
// not an error.
func TestUnrankedPlayerGetsAnEmptyStanding(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")

	rec := h.do(t, http.MethodGet, "/v1/leaderboards/memory/me", nil, player.Tokens.AccessToken)
	require.Equal(t, http.StatusOK, rec.Code)

	standing := decodeInto[standingBody](t, rec)
	require.Zero(t, standing.Rank)
	require.Zero(t, standing.Points)
	require.Empty(t, standing.Neighbours)
	require.NotNil(t, standing.Neighbours)
}

func TestEveryPeriodIsQueryable(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	h.playMemory(t, player.Tokens.AccessToken, 4)

	for _, period := range leaderboard.Periods {
		rec := h.do(t, http.MethodGet,
			"/v1/leaderboards/memory?period="+string(period), nil, "")
		require.Equal(t, http.StatusOK, rec.Code, period)

		page := decodeInto[pageBody](t, rec)
		require.Equal(t, string(period), page.Period)
		require.Len(t, page.Entries, 1, "the score should appear in every window")
	}
}

func TestBadQueryParametersAreRejected(t *testing.T) {
	h := newAuthHarness(t)

	for name, query := range map[string]string{
		"unknown period":  "?period=hourly",
		"limit too big":   "?limit=1000",
		"limit zero":      "?limit=0",
		"limit not int":   "?limit=lots",
		"negative offset": "?offset=-1",
	} {
		t.Run(name, func(t *testing.T) {
			rec := h.do(t, http.MethodGet, "/v1/leaderboards/memory"+query, nil, "")
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}
}

func TestLeaderboardForUnknownGameIsNotFound(t *testing.T) {
	h := newAuthHarness(t)

	rec := h.do(t, http.MethodGet, "/v1/leaderboards/pinball", nil, "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestLeaderboardPaginates(t *testing.T) {
	h := newAuthHarness(t)

	for i := range 5 {
		player := h.register(t, "player"+string(rune('a'+i)))
		h.playMemory(t, player.Tokens.AccessToken, i+1)
	}

	first := decodeInto[pageBody](t,
		h.do(t, http.MethodGet, "/v1/leaderboards/memory?limit=2", nil, ""))
	require.Len(t, first.Entries, 2)
	require.Equal(t, int64(5), first.Total)

	second := decodeInto[pageBody](t,
		h.do(t, http.MethodGet, "/v1/leaderboards/memory?limit=2&offset=2", nil, ""))
	require.Len(t, second.Entries, 2)
	require.Equal(t, 2, second.Offset)

	require.NotEqual(t, first.Entries[0].Username, second.Entries[0].Username)
	require.LessOrEqual(t, second.Entries[0].Points, first.Entries[1].Points)
}

// Playing again with a worse result must not demote a personal best.
func TestAWorseSecondAttemptDoesNotLowerYourBest(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	token := player.Tokens.AccessToken

	best := h.playMemory(t, token, 6)
	h.playMemory(t, token, 1)

	page := decodeInto[pageBody](t,
		h.do(t, http.MethodGet, "/v1/leaderboards/memory", nil, ""))

	require.Len(t, page.Entries, 1, "a player appears once, not once per session")
	require.Equal(t, best.Score.Points, page.Entries[0].Points)
}
