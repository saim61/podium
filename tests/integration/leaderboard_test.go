package integration

import (
	"math/rand/v2"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/leaderboard"
	"github.com/saim61/podium/internal/platform/redis"
	"github.com/saim61/podium/internal/store/db"
	"github.com/saim61/podium/internal/testsupport"
	"github.com/saim61/podium/internal/user"
)

type boardFixture struct {
	board *leaderboard.Board
	pool  *pgxpool.Pool
	redis *redis.Client
	at    time.Time
}

func newBoard(t *testing.T) *boardFixture {
	t.Helper()

	pool := testsupport.Postgres(t)
	rdb := testsupport.Redis(t)

	fixed := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	board, err := leaderboard.New(rdb, user.NewDirectory(pool),
		leaderboard.WithClock(func() time.Time { return fixed }))
	require.NoError(t, err)

	return &boardFixture{board: board, pool: pool, redis: rdb, at: fixed}
}

// player creates an account so leaderboard entries have a name to resolve to.
func (f *boardFixture) player(t *testing.T, username string) int64 {
	t.Helper()

	created, err := db.New(f.pool).CreateUser(t.Context(), db.CreateUserParams{
		Username:     username,
		Email:        username + "@example.com",
		PasswordHash: "x",
	})
	require.NoError(t, err)
	return created.ID
}

func (f *boardFixture) submit(t *testing.T, userID int64, game games.Slug, points int) leaderboard.Placement {
	t.Helper()

	placement, err := f.board.Submit(t.Context(), userID, game, points, f.at)
	require.NoError(t, err)
	return placement
}

func (f *boardFixture) score(t *testing.T, key string, userID int64) float64 {
	t.Helper()

	score, err := f.redis.ZScore(t.Context(), key, strconv.FormatInt(userID, 10)).Result()
	require.NoError(t, err, key)
	return score
}

func TestSubmitPlacesAPlayerOnEveryPeriod(t *testing.T) {
	f := newBoard(t)
	saeem := f.player(t, "saeem")

	placement := f.submit(t, saeem, games.Memory, 4000)

	for _, period := range leaderboard.Periods {
		require.True(t, placement.Improved[period], period)
		require.Equal(t, 4000, placement.Game[period].Points, period)
		require.Equal(t, int64(1), placement.Game[period].Rank, period)
		require.Equal(t, 4000, placement.Global[period].Points, period)
	}
}

// A game's board keeps a personal best, not the latest attempt.
func TestOnlyABetterScoreReplacesTheOldOne(t *testing.T) {
	f := newBoard(t)
	saeem := f.player(t, "saeem")

	f.submit(t, saeem, games.Memory, 5000)
	worse := f.submit(t, saeem, games.Memory, 3000)

	require.False(t, worse.Improved[leaderboard.AllTime])
	require.Equal(t, 5000, worse.Game[leaderboard.AllTime].Points)
	require.Equal(t, float64(5000), f.score(t, "lb:g:memory:all", saeem))

	better := f.submit(t, saeem, games.Memory, 7000)
	require.True(t, better.Improved[leaderboard.AllTime])
	require.Equal(t, 7000, better.Game[leaderboard.AllTime].Points)
}

// The global board is the SUM of per-game bests, so improving one game must move it by exactly
// the difference - not by the whole new score.
func TestGlobalScoreMovesByTheDeltaOnly(t *testing.T) {
	f := newBoard(t)
	saeem := f.player(t, "saeem")

	f.submit(t, saeem, games.Memory, 5000)
	require.Equal(t, float64(5000), f.score(t, "lb:global:all", saeem))

	f.submit(t, saeem, games.Memory, 8000)
	require.Equal(t, float64(8000), f.score(t, "lb:global:all", saeem),
		"improving 5000 to 8000 must add 3000, not 8000")

	f.submit(t, saeem, games.Memory, 6000)
	require.Equal(t, float64(8000), f.score(t, "lb:global:all", saeem),
		"a worse score must not change the global total at all")
}

func TestGlobalScoreSumsAcrossGames(t *testing.T) {
	f := newBoard(t)
	saeem := f.player(t, "saeem")

	expected := 0
	for i, slug := range games.NewRegistry().Slugs() {
		points := 1000 * (i + 1)
		f.submit(t, saeem, slug, points)
		expected += points
	}

	require.Equal(t, float64(expected), f.score(t, "lb:global:all", saeem))
	require.Equal(t, float64(15000), f.score(t, "lb:global:all", saeem))
}

// This is the test the Lua script exists for. Without atomicity, concurrent submissions read the
// same previous score and each add their own delta, permanently inflating the global total.
func TestConcurrentSubmissionsKeepGlobalConsistent(t *testing.T) {
	f := newBoard(t)

	players := make([]int64, 6)
	for i := range players {
		players[i] = f.player(t, "player"+strconv.Itoa(i))
	}

	slugs := games.NewRegistry().Slugs()

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for range 8 {
				userID := players[rand.IntN(len(players))]
				slug := slugs[rand.IntN(len(slugs))]

				_, err := f.board.Submit(t.Context(), userID, slug, rand.IntN(games.MaxPoints), f.at)
				require.NoError(t, err)
			}
		}()
	}
	wg.Wait()

	for _, period := range leaderboard.Periods {
		for _, userID := range players {
			var expected float64
			for _, slug := range slugs {
				score, err := f.redis.ZScore(t.Context(),
					leaderboard.Game(slug).Key(period, f.at),
					strconv.FormatInt(userID, 10)).Result()
				if err == nil {
					expected += score
				}
			}

			actual := f.score(t, leaderboard.Global().Key(period, f.at), userID)
			require.Equal(t, expected, actual,
				"global total for user %d in %s must equal the sum of per-game bests",
				userID, period)
		}
	}
}

func TestPeriodKeysExpireAndAllTimeDoesNot(t *testing.T) {
	f := newBoard(t)
	saeem := f.player(t, "saeem")

	f.submit(t, saeem, games.Memory, 4000)

	forever, err := f.redis.TTL(t.Context(), "lb:g:memory:all").Result()
	require.NoError(t, err)
	require.Negative(t, forever, "the all-time board must never expire")

	for _, period := range []leaderboard.Period{leaderboard.Daily, leaderboard.Weekly, leaderboard.Monthly} {
		for _, key := range []string{
			leaderboard.Game(games.Memory).Key(period, f.at),
			leaderboard.Global().Key(period, f.at),
		} {
			ttl, err := f.redis.TTL(t.Context(), key).Result()
			require.NoError(t, err, key)
			require.Positive(t, ttl, key)
			require.LessOrEqual(t, ttl, period.TTL(), key)
		}
	}
}

// A second write must not push the expiry out, or a daily board would never roll over for an
// active player.
func TestSubmittingAgainDoesNotExtendTheWindow(t *testing.T) {
	f := newBoard(t)
	saeem := f.player(t, "saeem")

	key := leaderboard.Game(games.Memory).Key(leaderboard.Daily, f.at)

	f.submit(t, saeem, games.Memory, 1000)
	first, err := f.redis.PTTL(t.Context(), key).Result()
	require.NoError(t, err)

	time.Sleep(30 * time.Millisecond)

	f.submit(t, saeem, games.Memory, 2000)
	second, err := f.redis.PTTL(t.Context(), key).Result()
	require.NoError(t, err)

	require.Less(t, second, first, "the expiry should keep counting down, not reset")
}

// Competition ranking: everyone on the same score shares a rank, and the next distinct score
// skips the places they occupy.
func TestTiedScoresShareARank(t *testing.T) {
	f := newBoard(t)

	top := f.player(t, "top")
	tiedA := f.player(t, "tieda")
	tiedB := f.player(t, "tiedb")
	last := f.player(t, "last")

	f.submit(t, top, games.Memory, 9000)
	f.submit(t, tiedA, games.Memory, 5000)
	f.submit(t, tiedB, games.Memory, 5000)
	f.submit(t, last, games.Memory, 1000)

	page, err := f.board.Page(t.Context(), leaderboard.Game(games.Memory), leaderboard.AllTime, 0, 10)
	require.NoError(t, err)
	require.Len(t, page.Entries, 4)
	require.Equal(t, int64(4), page.Total)

	require.Equal(t, int64(1), page.Entries[0].Rank)
	require.Equal(t, int64(2), page.Entries[1].Rank)
	require.Equal(t, int64(2), page.Entries[2].Rank, "tied players share a rank")
	require.Equal(t, int64(4), page.Entries[3].Rank, "the next score skips the tied place")
}

func TestPageResolvesUsernames(t *testing.T) {
	f := newBoard(t)

	saeem := f.player(t, "saeem")
	other := f.player(t, "someone")

	f.submit(t, saeem, games.Memory, 9000)
	f.submit(t, other, games.Memory, 8000)

	page, err := f.board.Page(t.Context(), leaderboard.Game(games.Memory), leaderboard.AllTime, 0, 10)
	require.NoError(t, err)

	require.Equal(t, "saeem", page.Entries[0].Username)
	require.Equal(t, "someone", page.Entries[1].Username)

	// Names are cached in Redis so a busy board does not query Postgres per row.
	cached, err := f.redis.Get(t.Context(), "user:name:"+strconv.FormatInt(saeem, 10)).Result()
	require.NoError(t, err)
	require.Equal(t, "saeem", cached)
}

func TestPageIsOrderedAndPaged(t *testing.T) {
	f := newBoard(t)

	for i := range 7 {
		id := f.player(t, "p"+strconv.Itoa(i))
		f.submit(t, id, games.Memory, (i+1)*1000)
	}

	first, err := f.board.Page(t.Context(), leaderboard.Game(games.Memory), leaderboard.AllTime, 0, 3)
	require.NoError(t, err)
	require.Len(t, first.Entries, 3)
	require.Equal(t, 7000, first.Entries[0].Points)
	require.Equal(t, 5000, first.Entries[2].Points)
	require.Equal(t, int64(1), first.Entries[0].Rank)

	second, err := f.board.Page(t.Context(), leaderboard.Game(games.Memory), leaderboard.AllTime, 3, 3)
	require.NoError(t, err)
	require.Len(t, second.Entries, 3)
	require.Equal(t, 4000, second.Entries[0].Points)
	require.Equal(t, int64(4), second.Entries[0].Rank, "ranks must continue across pages")
	require.Equal(t, 3, second.Offset)
}

// A page can begin part way through a tie, which is the one case the single ZCOUNT has to get
// right.
func TestPageStartingInsideATieKeepsTheSharedRank(t *testing.T) {
	f := newBoard(t)

	f.submit(t, f.player(t, "lead"), games.Memory, 9000)
	for i := range 3 {
		f.submit(t, f.player(t, "tied"+strconv.Itoa(i)), games.Memory, 5000)
	}
	f.submit(t, f.player(t, "tail"), games.Memory, 1000)

	page, err := f.board.Page(t.Context(), leaderboard.Game(games.Memory), leaderboard.AllTime, 2, 3)
	require.NoError(t, err)
	require.Len(t, page.Entries, 3)

	require.Equal(t, 5000, page.Entries[0].Points)
	require.Equal(t, int64(2), page.Entries[0].Rank,
		"a page opening mid-tie must still report the tie's rank")
	require.Equal(t, int64(2), page.Entries[1].Rank)
	require.Equal(t, int64(5), page.Entries[2].Rank)
}

func TestEmptyBoardReturnsNoEntries(t *testing.T) {
	f := newBoard(t)

	page, err := f.board.Page(t.Context(), leaderboard.Game(games.Reaction), leaderboard.AllTime, 0, 10)

	require.NoError(t, err)
	require.Zero(t, page.Total)
	require.Empty(t, page.Entries)
	require.NotNil(t, page.Entries, "an empty board should marshal as [] not null")
}

func TestStandingReportsRankAndNeighbours(t *testing.T) {
	f := newBoard(t)

	var ids []int64
	for i := range 7 {
		id := f.player(t, "p"+strconv.Itoa(i))
		ids = append(ids, id)
		f.submit(t, id, games.Memory, (i+1)*1000)
	}

	middle := ids[3]

	standing, neighbours, err := f.board.Standing(t.Context(),
		leaderboard.Game(games.Memory), leaderboard.AllTime, middle, 2)

	require.NoError(t, err)
	require.Equal(t, 4000, standing.Points)
	require.Equal(t, int64(4), standing.Rank)
	require.Len(t, neighbours, 5, "two either side plus the player")
	require.Equal(t, middle, neighbours[2].UserID)
}

func TestStandingOfAnUnrankedPlayer(t *testing.T) {
	f := newBoard(t)
	saeem := f.player(t, "saeem")

	_, _, err := f.board.Standing(t.Context(),
		leaderboard.Game(games.Memory), leaderboard.AllTime, saeem, 2)

	require.ErrorIs(t, err, leaderboard.ErrUnranked)
}

func TestGameBoardsAreIndependent(t *testing.T) {
	f := newBoard(t)
	saeem := f.player(t, "saeem")

	f.submit(t, saeem, games.Memory, 9000)

	memory, err := f.board.Page(t.Context(), leaderboard.Game(games.Memory), leaderboard.AllTime, 0, 10)
	require.NoError(t, err)
	require.Len(t, memory.Entries, 1)

	reaction, err := f.board.Page(t.Context(), leaderboard.Game(games.Reaction), leaderboard.AllTime, 0, 10)
	require.NoError(t, err)
	require.Empty(t, reaction.Entries, "a score in one game must not appear in another")
}

func TestZeroPointsStillRanksAPlayer(t *testing.T) {
	f := newBoard(t)
	saeem := f.player(t, "saeem")

	// Quitting a game scores nothing, but the player has still played and belongs on the board.
	f.submit(t, saeem, games.NumberGuess, 0)

	page, err := f.board.Page(t.Context(), leaderboard.Game(games.NumberGuess), leaderboard.AllTime, 0, 10)
	require.NoError(t, err)
	require.Len(t, page.Entries, 1)
	require.Equal(t, 0, page.Entries[0].Points)
	require.Equal(t, int64(1), page.Entries[0].Rank)
}

func TestScoresLandInTheRightPeriodBucket(t *testing.T) {
	f := newBoard(t)
	saeem := f.player(t, "saeem")

	f.submit(t, saeem, games.Memory, 4000)

	// The fixed clock is 2026-09-07, a Monday in ISO week 37.
	for _, key := range []string{
		"lb:g:memory:all",
		"lb:g:memory:d:2026-09-07",
		"lb:g:memory:w:2026-W37",
		"lb:g:memory:m:2026-09",
	} {
		exists, err := f.redis.Exists(t.Context(), key).Result()
		require.NoError(t, err)
		require.Equal(t, int64(1), exists, key)
	}
}
