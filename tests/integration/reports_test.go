package integration

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/leaderboard"
	"github.com/saim61/podium/internal/projector"
	"github.com/saim61/podium/internal/reports"
	"github.com/saim61/podium/internal/scores"
)

type reportFixture struct {
	*boardFixture
	service *reports.Service
}

func newReports(t *testing.T) *reportFixture {
	t.Helper()

	f := newBoard(t)

	return &reportFixture{
		boardFixture: f,
		service: reports.NewService(f.pool, f.board, games.NewRegistry(),
			reports.WithClock(func() time.Time { return f.at })),
	}
}

func (f *reportFixture) report(t *testing.T, scope leaderboard.Scope, period leaderboard.Period, at time.Time) reports.Report {
	t.Helper()

	report, err := f.service.TopPlayers(t.Context(), reports.Request{
		Scope:  scope,
		Period: period,
		At:     at,
		Limit:  10,
	})
	require.NoError(t, err)
	return report
}

func TestReportReadsRedisWhileTheWindowIsLive(t *testing.T) {
	f := newReports(t)

	alice := f.player(t, "alice")
	bob := f.player(t, "bob")
	f.submit(t, alice, games.Memory, 8000)
	f.submit(t, bob, games.Memory, 3000)

	report := f.report(t, leaderboard.Game(games.Memory), leaderboard.Daily, f.at)

	require.Equal(t, reports.SourceLive, report.Source)
	require.Equal(t, "memory", report.Scope)
	require.Equal(t, 2, report.Players)
	require.Equal(t, "alice", report.Entries[0].Username)
	require.Equal(t, 8000, report.Entries[0].Points)
	require.NotNil(t, report.From)
	require.NotNil(t, report.Until)
}

// The whole point of the split: an old window and a live one must rank people identically. If
// they disagreed, the same day would have two different answers depending only on when you
// asked.
func TestLiveAndHistoryReportsAgree(t *testing.T) {
	for _, scope := range []leaderboard.Scope{leaderboard.Game(games.Memory), leaderboard.Global()} {
		t.Run(scope.Name(), func(t *testing.T) {
			// A fixture per subtest: the flush below empties Redis, so a shared one would leave
			// the second subtest with nothing live to compare against.
			f := newReports(t)

			for i, name := range []string{"alice", "bob", "carol", "dave"} {
				id := f.player(t, name)
				f.submit(t, id, games.Memory, (i+1)*1500)
				f.submit(t, id, games.Reaction, (4-i)*1000)
			}

			live := f.report(t, scope, leaderboard.Daily, f.at)
			require.Equal(t, reports.SourceLive, live.Source)

			// Drop the sorted sets, so the only store left holding the window is Postgres.
			require.NoError(t, f.redis.FlushDB(t.Context()).Err())

			cold := f.report(t, scope, leaderboard.Daily, f.at)
			require.Equal(t, reports.SourceHistory, cold.Source)

			require.Equal(t, live.Players, cold.Players)
			require.Equal(t, live.Entries, cold.Entries,
				"the cold path must reproduce the live ranking exactly")
		})
	}
}

func TestHistoryReportMatchesLiveForEveryPeriod(t *testing.T) {
	f := newReports(t)

	alice := f.player(t, "alice")
	bob := f.player(t, "bob")
	f.submit(t, alice, games.MathSprint, 7000)
	f.submit(t, bob, games.MathSprint, 7000)
	f.submit(t, bob, games.Memory, 2000)

	live := map[leaderboard.Period]reports.Report{}
	for _, period := range leaderboard.Periods {
		live[period] = f.report(t, leaderboard.Global(), period, f.at)
	}

	require.NoError(t, f.redis.FlushDB(t.Context()).Err())

	for _, period := range leaderboard.Periods {
		cold := f.report(t, leaderboard.Global(), period, f.at)

		require.Equal(t, reports.SourceHistory, cold.Source, period)
		require.Equal(t, live[period].Entries, cold.Entries, period)
	}
}

// Ties must share a rank on the cold path too, or the shape of the answer would give away which
// store served it.
func TestColdPathUsesCompetitionRanks(t *testing.T) {
	f := newReports(t)

	top := f.player(t, "top")
	tiedA := f.player(t, "tieda")
	tiedB := f.player(t, "tiedb")
	last := f.player(t, "last")

	f.submit(t, top, games.Memory, 9000)
	f.submit(t, tiedA, games.Memory, 5000)
	f.submit(t, tiedB, games.Memory, 5000)
	f.submit(t, last, games.Memory, 1000)

	require.NoError(t, f.redis.FlushDB(t.Context()).Err())

	report := f.report(t, leaderboard.Game(games.Memory), leaderboard.Daily, f.at)

	require.Equal(t, reports.SourceHistory, report.Source)
	require.Equal(t, int64(1), report.Entries[0].Rank)
	require.Equal(t, int64(2), report.Entries[1].Rank)
	require.Equal(t, int64(2), report.Entries[2].Rank)
	require.Equal(t, int64(4), report.Entries[3].Rank)
}

func TestReportForAWindowNobodyPlayedIsEmptyNotAnError(t *testing.T) {
	f := newReports(t)

	lastYear := f.at.AddDate(-1, 0, 0)
	report := f.report(t, leaderboard.Game(games.Memory), leaderboard.Daily, lastYear)

	require.Equal(t, reports.SourceHistory, report.Source)
	require.Zero(t, report.Players)
	require.Empty(t, report.Entries)
	require.NotNil(t, report.Entries)
}

func TestReportWindowBoundariesAreRespected(t *testing.T) {
	f := newReports(t)
	alice := f.player(t, "alice")
	f.submit(t, alice, games.Memory, 5000)

	require.NoError(t, f.redis.FlushDB(t.Context()).Err())

	today := f.report(t, leaderboard.Game(games.Memory), leaderboard.Daily, f.at)
	require.Len(t, today.Entries, 1)

	// The score was recorded today, so yesterday's window must not see it.
	yesterday := f.report(t, leaderboard.Game(games.Memory), leaderboard.Daily, f.at.AddDate(0, 0, -1))
	require.Empty(t, yesterday.Entries)
}

func TestMaterialisingAClosedWindowMakesItCheapToRead(t *testing.T) {
	f := newReports(t)

	alice := f.player(t, "alice")
	bob := f.player(t, "bob")

	// Two scores recorded inside yesterday's window.
	yesterday := f.at.AddDate(0, 0, -1)
	f.recordAt(t, alice, games.Memory, 6000, yesterday)
	f.recordAt(t, bob, games.Memory, 2000, yesterday)

	// Nothing in Redis for that window, so it reads from history.
	before := f.report(t, leaderboard.Game(games.Memory), leaderboard.Daily, yesterday)
	require.Equal(t, reports.SourceHistory, before.Source)
	require.Len(t, before.Entries, 2)

	written, err := f.service.MaterialiseClosedWindows(t.Context(), f.at)
	require.NoError(t, err)
	require.Positive(t, written.Written)

	after := f.report(t, leaderboard.Game(games.Memory), leaderboard.Daily, yesterday)

	require.Equal(t, reports.SourceSnapshot, after.Source,
		"a frozen window should be served from its snapshot")
	require.Equal(t, before.Players, after.Players)
	require.Equal(t, before.Entries, after.Entries,
		"freezing a window must not change what it says")
}

// recordAt writes an authoritative score at a chosen instant, so a test can populate a window
// that has already closed.
func (f *reportFixture) recordAt(t *testing.T, userID int64, game games.Slug, points int, at time.Time) {
	t.Helper()

	var sessionID string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		INSERT INTO game_sessions (user_id, game, seed, state, status, started_at, finished_at)
		VALUES ($1, $2, 1, '{}', 'finished', $3, $3)
		RETURNING id`, userID, string(game), at).Scan(&sessionID))

	_, err := f.pool.Exec(t.Context(), `
		INSERT INTO score_events (user_id, session_id, game, raw, points, achieved_at, projected_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`,
		userID, sessionID, string(game), float64(points), points, at)
	require.NoError(t, err)
}

func TestMaterialisingIsIdempotent(t *testing.T) {
	f := newReports(t)

	alice := f.player(t, "alice")
	f.recordAt(t, alice, games.Memory, 6000, f.at.AddDate(0, 0, -1))

	first, err := f.service.MaterialiseClosedWindows(t.Context(), f.at)
	require.NoError(t, err)
	require.Positive(t, first.Written)

	second, err := f.service.MaterialiseClosedWindows(t.Context(), f.at)
	require.NoError(t, err)
	require.Equal(t, first.Written, second.Written)

	var rows int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM leaderboard_snapshots").Scan(&rows))
	require.Equal(t, first.Written, rows, "a second pass must update rows, not duplicate them")
}

// A late score arriving after the first pass has to be picked up, or a frozen window would be
// permanently wrong.
func TestMaterialisingAgainPicksUpALateScore(t *testing.T) {
	f := newReports(t)

	yesterday := f.at.AddDate(0, 0, -1)
	alice := f.player(t, "alice")
	f.recordAt(t, alice, games.Memory, 3000, yesterday)

	_, err := f.service.MaterialiseClosedWindows(t.Context(), f.at)
	require.NoError(t, err)

	bob := f.player(t, "bob")
	f.recordAt(t, bob, games.Memory, 9000, yesterday)

	_, err = f.service.MaterialiseClosedWindows(t.Context(), f.at)
	require.NoError(t, err)

	report := f.report(t, leaderboard.Game(games.Memory), leaderboard.Daily, yesterday)
	require.Equal(t, reports.SourceSnapshot, report.Source)
	require.Len(t, report.Entries, 2)
	require.Equal(t, "bob", report.Entries[0].Username)
}

func TestMaterialisingSkipsWindowsWithNoPlayers(t *testing.T) {
	f := newReports(t)

	written, err := f.service.MaterialiseClosedWindows(t.Context(), f.at)

	require.NoError(t, err)
	require.Zero(t, written.Written)
	require.Positive(t, written.Skipped)

	var rows int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM leaderboard_snapshots").Scan(&rows))
	require.Zero(t, rows, "an empty window is not worth a row")
}

func TestHousekeeperMaterialisesClosedWindows(t *testing.T) {
	f := newReports(t)

	alice := f.player(t, "alice")
	f.recordAt(t, alice, games.Memory, 4000, f.at.AddDate(0, 0, -1))

	house := projector.NewHousekeeper(scores.NewStore(f.pool), workerConfig(), discard(), f.service)
	require.NoError(t, house.Sweep(t.Context()))

	var rows int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM leaderboard_snapshots").Scan(&rows))
	require.Positive(t, rows)
}

func TestReportsOverHTTP(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")

	finished := h.playMemory(t, player.Tokens.AccessToken, 5)

	rec := h.do(t, http.MethodGet, "/v1/reports/top-players?period=daily&game=memory", nil, "")
	require.Equal(t, http.StatusOK, rec.Code)

	body := decodeInto[struct {
		Scope   string `json:"scope"`
		Period  string `json:"period"`
		Source  string `json:"source"`
		Players int    `json:"players"`
		From    string `json:"from"`
		Entries []struct {
			Rank     int64  `json:"rank"`
			Username string `json:"username"`
			Points   int    `json:"points"`
		} `json:"entries"`
	}](t, rec)

	require.Equal(t, "memory", body.Scope)
	require.Equal(t, "daily", body.Period)
	require.Equal(t, "live", body.Source)
	require.Equal(t, 1, body.Players)
	require.NotEmpty(t, body.From)
	require.Equal(t, "saeem", body.Entries[0].Username)
	require.Equal(t, finished.Score.Points, body.Entries[0].Points)
}

func TestReportDefaultsToGlobalAllTime(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	h.playMemory(t, player.Tokens.AccessToken, 3)

	rec := h.do(t, http.MethodGet, "/v1/reports/top-players", nil, "")
	require.Equal(t, http.StatusOK, rec.Code)

	body := decodeInto[struct {
		Scope  string `json:"scope"`
		Period string `json:"period"`
		From   string `json:"from"`
	}](t, rec)

	require.Equal(t, "global", body.Scope)
	require.Equal(t, "all-time", body.Period)
	require.Empty(t, body.From, "all-time has no window, so it should report none")
}

func TestReportRejectsBadParameters(t *testing.T) {
	h := newAuthHarness(t)

	for name, query := range map[string]string{
		"unknown period": "?period=hourly",
		"bad date":       "?date=last-tuesday",
		"partial date":   "?date=2026-09",
		"limit too big":  "?limit=5000",
		"limit zero":     "?limit=0",
	} {
		t.Run(name, func(t *testing.T) {
			rec := h.do(t, http.MethodGet, "/v1/reports/top-players"+query, nil, "")
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}
}

func TestReportForUnknownGameIsNotFound(t *testing.T) {
	h := newAuthHarness(t)

	rec := h.do(t, http.MethodGet, "/v1/reports/top-players?game=pinball", nil, "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestReportAcceptsAnExplicitDate(t *testing.T) {
	h := newAuthHarness(t)

	date := time.Now().UTC().AddDate(0, 0, -3).Format(time.DateOnly)
	rec := h.do(t, http.MethodGet,
		"/v1/reports/top-players?period=daily&date="+date, nil, "")

	require.Equal(t, http.StatusOK, rec.Code)

	body := decodeInto[struct {
		From   string `json:"from"`
		Source string `json:"source"`
	}](t, rec)

	require.Contains(t, body.From, date, "the window should be the one containing that date")
}

func TestPublicReadsAreRateLimited(t *testing.T) {
	h := newAuthHarness(t)

	limit := h.cfg.Auth.ReadsPerWindow
	require.Positive(t, limit)

	var lastCode int
	for i := 0; i <= limit; i++ {
		lastCode = h.do(t, http.MethodGet, "/v1/leaderboards/global", nil, "").Code
	}

	require.Equal(t, http.StatusTooManyRequests, lastCode,
		"an unauthenticated read still needs a ceiling")
}

func TestRateLimitHeadersAreReported(t *testing.T) {
	h := newAuthHarness(t)

	rec := h.do(t, http.MethodGet, "/v1/leaderboards/global", nil, "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, strconv.Itoa(h.cfg.Auth.ReadsPerWindow), rec.Header().Get("RateLimit-Limit"))
	require.NotEmpty(t, rec.Header().Get("RateLimit-Remaining"))
}

// Two accounts must not share a budget, or one busy player would lock everyone else out.
func TestAuthenticatedCallersAreMeteredSeparately(t *testing.T) {
	h := newAuthHarness(t)

	first := h.register(t, "playerone")
	second := h.register(t, "playertwo")

	opened := h.startSession(t, first.Tokens.AccessToken, games.Memory)

	limit := h.cfg.Auth.MovesPerWindow
	for i := 0; i < limit+1; i++ {
		h.do(t, http.MethodPost, "/v1/sessions/"+opened.ID.String()+"/moves",
			map[string][]int{"answer": {1}}, first.Tokens.AccessToken)
	}

	exhausted := h.do(t, http.MethodPost, "/v1/sessions/"+opened.ID.String()+"/moves",
		map[string][]int{"answer": {1}}, first.Tokens.AccessToken)
	require.Equal(t, http.StatusTooManyRequests, exhausted.Code)

	// The second account is untouched.
	others := h.startSession(t, second.Tokens.AccessToken, games.Memory)
	require.NotEqual(t, http.StatusTooManyRequests,
		h.do(t, http.MethodPost, "/v1/sessions/"+others.ID.String()+"/moves",
			map[string][]int{"answer": {1}}, second.Tokens.AccessToken).Code)
}
