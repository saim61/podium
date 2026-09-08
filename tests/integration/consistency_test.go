package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/leaderboard"
	"github.com/saim61/podium/internal/projector"
	"github.com/saim61/podium/internal/scores"
	"github.com/saim61/podium/internal/session"
	"github.com/saim61/podium/internal/user"
)

func discard() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func workerConfig() config.Worker {
	return config.Worker{
		ProjectorInterval:    10 * time.Millisecond,
		ProjectorBatch:       100,
		HousekeepingInterval: time.Hour,
		SessionMaxAge:        time.Hour,
	}
}

// boardSnapshot reads every leaderboard key and its members, so a before/after comparison can
// assert a rebuild reproduced the boards exactly rather than merely populating something.
func (f *boardFixture) snapshot(t *testing.T) map[string]map[string]float64 {
	t.Helper()

	snapshot := map[string]map[string]float64{}

	keys, err := f.redis.Keys(t.Context(), "lb:*").Result()
	require.NoError(t, err)

	for _, key := range keys {
		rows, err := f.redis.ZRangeWithScores(t.Context(), key, 0, -1).Result()
		require.NoError(t, err)

		members := map[string]float64{}
		for _, row := range rows {
			members[row.Member.(string)] = row.Score
		}
		snapshot[key] = members
	}
	return snapshot
}

// The claim this phase exists to prove: Redis is disposable. Flush it entirely and one rebuild
// restores every board, byte for byte.
func TestFlushingRedisLosesNothingRebuildCannotRestore(t *testing.T) {
	f := newBoard(t)

	alice := f.player(t, "alice")
	bob := f.player(t, "bob")
	carol := f.player(t, "carol")

	f.submit(t, alice, games.Memory, 8000)
	f.submit(t, alice, games.Memory, 4000)
	f.submit(t, alice, games.MathSprint, 6000)
	f.submit(t, bob, games.Memory, 8000)
	f.submit(t, bob, games.Reaction, 2500)
	f.submit(t, carol, games.NumberGuess, 0)

	before := f.snapshot(t)
	require.NotEmpty(t, before)

	require.NoError(t, f.redis.FlushDB(t.Context()).Err())
	require.Empty(t, f.snapshot(t), "the boards really are gone")

	report, err := f.board.Rebuild(t.Context(), f.history(t))
	require.NoError(t, err)
	require.Positive(t, report.Entries)

	require.Equal(t, before, f.snapshot(t),
		"a rebuild must reproduce every board exactly as it was")
}

// history builds the Postgres-backed score reader a rebuild reads from. The scores must already
// exist as score_events rows, so this fixture writes them alongside the Redis submissions.
func (f *boardFixture) history(t *testing.T) leaderboard.History {
	t.Helper()
	return scores.NewStore(f.pool)
}

// A rebuild reads Postgres, so the scores have to be there. This plays real sessions through the
// service so score_events is populated the way production populates it.
type playFixture struct {
	sessions *session.Service
	board    *leaderboard.Board
	store    *scores.Store
	fixture  *boardFixture
}

func newPlayFixture(t *testing.T) *playFixture {
	t.Helper()

	f := newBoard(t)
	return &playFixture{
		sessions: session.NewService(f.pool, games.NewRegistry(),
			session.WithProjector(f.board)),
		board:   f.board,
		store:   scores.NewStore(f.pool),
		fixture: f,
	}
}

func (p *playFixture) play(t *testing.T, userID int64, slug games.Slug) session.Session {
	t.Helper()

	opened, err := p.sessions.Start(t.Context(), userID, slug)
	require.NoError(t, err)

	finished, err := p.sessions.Finish(t.Context(), userID, opened.ID)
	require.NoError(t, err)
	return finished
}

func TestRebuildRestoresBoardsFromRealPlay(t *testing.T) {
	p := newPlayFixture(t)
	f := p.fixture

	alice := f.player(t, "alice")
	bob := f.player(t, "bob")

	for _, slug := range games.NewRegistry().Slugs() {
		p.play(t, alice, slug)
	}
	p.play(t, bob, games.Memory)

	before := f.snapshot(t)
	require.NoError(t, f.redis.FlushDB(t.Context()).Err())

	_, err := f.board.Rebuild(t.Context(), p.store)
	require.NoError(t, err)

	require.Equal(t, before, f.snapshot(t))
}

func TestRebuildIsIdempotent(t *testing.T) {
	f := newBoard(t)
	saeem := f.player(t, "saeem")
	f.submit(t, saeem, games.Memory, 5000)

	_, err := f.board.Rebuild(t.Context(), f.history(t))
	require.NoError(t, err)
	first := f.snapshot(t)

	_, err = f.board.Rebuild(t.Context(), f.history(t))
	require.NoError(t, err)

	require.Equal(t, first, f.snapshot(t))
}

// The reason a rebuild is worth having even when nothing was lost: it recomputes the cross-game
// total from scratch, so any drift the incremental path introduced is corrected.
func TestRebuildRepairsCorruptedGlobalTotals(t *testing.T) {
	f := newBoard(t)
	saeem := f.player(t, "saeem")

	f.submit(t, saeem, games.Memory, 4000)
	f.submit(t, saeem, games.Reaction, 3000)

	correct := f.score(t, "lb:global:all", saeem)
	require.Equal(t, float64(7000), correct)

	// Simulate the exact damage a non-atomic submission would do.
	require.NoError(t, f.redis.ZIncrBy(t.Context(), "lb:global:all", 5000,
		strconv.FormatInt(saeem, 10)).Err())
	require.Equal(t, float64(12000), f.score(t, "lb:global:all", saeem))

	_, err := f.board.Rebuild(t.Context(), f.history(t))
	require.NoError(t, err)

	require.Equal(t, correct, f.score(t, "lb:global:all", saeem),
		"a rebuild recomputes the total and undoes the drift")
}

func TestRebuildRemovesEntriesThatShouldNotBeThere(t *testing.T) {
	f := newBoard(t)
	saeem := f.player(t, "saeem")
	f.submit(t, saeem, games.Memory, 4000)

	// A player who never scored, injected straight into the board.
	require.NoError(t, f.redis.ZAdd(t.Context(), "lb:g:memory:all",
		goredis.Z{Score: 9999, Member: "424242"}).Err())

	_, err := f.board.Rebuild(t.Context(), f.history(t))
	require.NoError(t, err)

	_, err = f.redis.ZScore(t.Context(), "lb:g:memory:all", "424242").Result()
	require.ErrorIs(t, err, goredis.Nil, "an entry with no score behind it must not survive")
}

func TestRebuildClearsABoardWithNoScores(t *testing.T) {
	f := newBoard(t)

	require.NoError(t, f.redis.ZAdd(t.Context(), "lb:g:reaction:all",
		goredis.Z{Score: 5000, Member: "1"}).Err())

	_, err := f.board.Rebuild(t.Context(), f.history(t))
	require.NoError(t, err)

	exists, err := f.redis.Exists(t.Context(), "lb:g:reaction:all").Result()
	require.NoError(t, err)
	require.Zero(t, exists, "a game nobody has played should have no board at all")
}

func TestRebuildPreservesPeriodExpiry(t *testing.T) {
	f := newBoard(t)
	saeem := f.player(t, "saeem")
	f.submit(t, saeem, games.Memory, 4000)

	_, err := f.board.Rebuild(t.Context(), f.history(t))
	require.NoError(t, err)

	forever, err := f.redis.TTL(t.Context(), "lb:g:memory:all").Result()
	require.NoError(t, err)
	require.Negative(t, forever)

	for _, period := range []leaderboard.Period{leaderboard.Daily, leaderboard.Weekly, leaderboard.Monthly} {
		ttl, err := f.redis.TTL(t.Context(), leaderboard.Game(games.Memory).Key(period, f.at)).Result()
		require.NoError(t, err)
		require.Positive(t, ttl, period)
	}
}

// Boards are built aside and RENAMEd into place, so no half-built set is ever visible - and no
// staging key is left lying around afterwards.
func TestRebuildLeavesNoStagingKeys(t *testing.T) {
	f := newBoard(t)
	saeem := f.player(t, "saeem")
	f.submit(t, saeem, games.Memory, 4000)

	_, err := f.board.Rebuild(t.Context(), f.history(t))
	require.NoError(t, err)

	staging, err := f.redis.Keys(t.Context(), "*:rebuilding").Result()
	require.NoError(t, err)
	require.Empty(t, staging)
}

// The projector's whole purpose: a score that never reached Redis becomes visible without
// anybody intervening.
func TestProjectorPublishesScoresRedisNeverReceived(t *testing.T) {
	f := newBoard(t)
	store := scores.NewStore(f.pool)

	// A session service whose leaderboard points at a closed port, so the inline projection
	// fails exactly as it would during a Redis outage.
	broken, err := leaderboard.New(mustDeadRedis(t), user.NewDirectory(f.pool))
	require.NoError(t, err)

	sessions := session.NewService(f.pool, games.NewRegistry(), session.WithProjector(broken))

	saeem := f.player(t, "saeem")
	opened, err := sessions.Start(t.Context(), saeem, games.Memory)
	require.NoError(t, err)

	finished, err := sessions.Finish(t.Context(), saeem, opened.ID)
	require.NoError(t, err, "a Redis outage must not fail a real score")
	require.NotNil(t, finished.Score)
	require.Nil(t, finished.Placement, "nothing was projected")

	pending, err := store.PendingCount(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), pending)

	require.Empty(t, f.snapshot(t), "the board is empty because the publish failed")

	// Now a healthy projector sweeps.
	projected, err := projector.New(store, f.board, workerConfig(), discard()).Sweep(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, projected)

	page, err := f.board.Page(t.Context(), leaderboard.Game(games.Memory), leaderboard.AllTime, 0, 10)
	require.NoError(t, err)
	require.Len(t, page.Entries, 1, "the score is on the board now")
	require.Equal(t, "saeem", page.Entries[0].Username)

	remaining, err := store.PendingCount(t.Context())
	require.NoError(t, err)
	require.Zero(t, remaining, "the score should be stamped as projected")
}

func mustDeadRedis(t *testing.T) *goredis.Client {
	t.Helper()

	return goredis.NewClient(&goredis.Options{
		Addr:            "127.0.0.1:1",
		MaxRetries:      -1,
		DialTimeout:     50 * time.Millisecond,
		ConnMaxIdleTime: time.Second,
	})
}

// At-least-once projection is only safe because a replay changes nothing. A game's board accepts
// a strictly better score only, so re-publishing the same score is a no-op and the cross-game
// total moves by a delta of zero.
func TestReprojectingAScoreChangesNothing(t *testing.T) {
	f := newBoard(t)
	store := scores.NewStore(f.pool)

	sessions := session.NewService(f.pool, games.NewRegistry(), session.WithProjector(f.board))

	saeem := f.player(t, "saeem")
	opened, err := sessions.Start(t.Context(), saeem, games.Memory)
	require.NoError(t, err)
	_, err = sessions.Finish(t.Context(), saeem, opened.ID)
	require.NoError(t, err)

	after := f.snapshot(t)

	// Pretend the stamp was lost, as it would be if the process died between publishing and
	// marking, then let the projector run again.
	_, err = f.pool.Exec(t.Context(), "UPDATE score_events SET projected_at = NULL")
	require.NoError(t, err)

	for range 3 {
		projected, err := projector.New(store, f.board, workerConfig(), discard()).Sweep(t.Context())
		require.NoError(t, err)
		require.Equal(t, 1, projected)

		_, err = f.pool.Exec(t.Context(), "UPDATE score_events SET projected_at = NULL")
		require.NoError(t, err)
	}

	require.Equal(t, after, f.snapshot(t),
		"replaying a projection must not inflate any board")
}

func TestSweepWithNothingPendingDoesNothing(t *testing.T) {
	f := newBoard(t)

	projected, err := projector.New(scores.NewStore(f.pool), f.board, workerConfig(), discard()).
		Sweep(t.Context())

	require.NoError(t, err)
	require.Zero(t, projected)
}

// A failed publish must leave the row unprojected, so the next sweep tries again rather than
// silently dropping it.
func TestFailedProjectionLeavesTheScorePending(t *testing.T) {
	f := newBoard(t)
	store := scores.NewStore(f.pool)

	sessions := session.NewService(f.pool, games.NewRegistry())

	saeem := f.player(t, "saeem")
	opened, err := sessions.Start(t.Context(), saeem, games.Memory)
	require.NoError(t, err)
	_, err = sessions.Finish(t.Context(), saeem, opened.ID)
	require.NoError(t, err)

	broken, err := leaderboard.New(mustDeadRedis(t), user.NewDirectory(f.pool))
	require.NoError(t, err)

	_, err = projector.New(store, broken, workerConfig(), discard()).Sweep(t.Context())
	require.Error(t, err)

	pending, err := store.PendingCount(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), pending, "an unpublished score must stay pending")
}

func TestProjectorRunSweepsUntilCancelled(t *testing.T) {
	f := newBoard(t)
	store := scores.NewStore(f.pool)

	sessions := session.NewService(f.pool, games.NewRegistry())
	saeem := f.player(t, "saeem")

	opened, err := sessions.Start(t.Context(), saeem, games.Memory)
	require.NoError(t, err)
	_, err = sessions.Finish(t.Context(), saeem, opened.ID)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- projector.New(store, f.board, workerConfig(), discard()).Run(ctx) }()

	require.Eventually(t, func() bool {
		pending, err := store.PendingCount(t.Context())
		return err == nil && pending == 0
	}, 5*time.Second, 20*time.Millisecond, "the loop should pick the score up on its own")

	cancel()

	select {
	case err := <-done:
		require.True(t, err == nil || errors.Is(err, context.Canceled))
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestHousekeeperRemovesExpiredRefreshTokens(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	require.NotEmpty(t, player.Tokens.RefreshToken)

	_, err := h.pool.Exec(t.Context(),
		"UPDATE refresh_tokens SET expires_at = now() - interval '1 day'")
	require.NoError(t, err)

	store := scores.NewStore(h.pool)
	require.NoError(t, projector.NewHousekeeper(store, workerConfig(), discard()).Sweep(t.Context()))

	var remaining int
	require.NoError(t, h.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM refresh_tokens").Scan(&remaining))
	require.Zero(t, remaining)
}

func TestHousekeeperAbandonsStaleSessions(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")

	opened := h.startSession(t, player.Tokens.AccessToken, games.Memory)

	_, err := h.pool.Exec(t.Context(),
		"UPDATE game_sessions SET started_at = now() - interval '1 day' WHERE id = $1", opened.ID)
	require.NoError(t, err)

	store := scores.NewStore(h.pool)
	require.NoError(t, projector.NewHousekeeper(store, workerConfig(), discard()).Sweep(t.Context()))

	require.Equal(t, session.StatusAbandoned, h.sessionStatus(t, opened.ID))
}

func TestHousekeeperLeavesLiveDataAlone(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	opened := h.startSession(t, player.Tokens.AccessToken, games.Memory)

	store := scores.NewStore(h.pool)
	require.NoError(t, projector.NewHousekeeper(store, workerConfig(), discard()).Sweep(t.Context()))

	require.Equal(t, session.StatusActive, h.sessionStatus(t, opened.ID))

	var tokens int
	require.NoError(t, h.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM refresh_tokens").Scan(&tokens))
	require.Equal(t, 1, tokens)

	rec := h.do(t, http.MethodGet, "/v1/me", nil, player.Tokens.AccessToken)
	require.Equal(t, http.StatusOK, rec.Code)
}
