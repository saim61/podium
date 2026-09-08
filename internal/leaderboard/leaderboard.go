package leaderboard

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/platform/redis"
)

//go:embed lua/*.lua
var scripts embed.FS

// Standing is where one user sits on one board.
type Standing struct {
	Points int   `json:"points"`
	Rank   int64 `json:"rank"`
}

// Placement is the result of recording a score: where the user now sits on the game's board and
// on the cross-game board, in every period.
type Placement struct {
	Improved map[Period]bool
	Game     map[Period]Standing
	Global   map[Period]Standing
}

// Entry is one row of a leaderboard page.
type Entry struct {
	Rank     int64  `json:"rank"`
	UserID   int64  `json:"-"`
	Username string `json:"username"`
	Points   int    `json:"points"`
}

// Page is a slice of a leaderboard.
type Page struct {
	Scope   string  `json:"scope"`
	Period  Period  `json:"period"`
	Total   int64   `json:"total"`
	Offset  int     `json:"offset"`
	Entries []Entry `json:"entries"`
}

// MaxPageSize caps how much of a board one request can read.
const MaxPageSize = 100

// Board reads and writes leaderboards.
type Board struct {
	client *redis.Client
	names  *nameResolver
	submit *goredis.Script
	now    func() time.Time
}

// Option adjusts a Board.
type Option func(*Board)

// WithClock replaces the time source, which decides which period buckets a score lands in.
func WithClock(now func() time.Time) Option {
	return func(b *Board) { b.now = now }
}

// New builds a Board. Names are hydrated through the resolver, which caches in Redis and falls
// back to Postgres.
func New(client *redis.Client, names Directory, opts ...Option) (*Board, error) {
	source, err := scripts.ReadFile("lua/submit.lua")
	if err != nil {
		return nil, fmt.Errorf("read submit script: %w", err)
	}

	b := &Board{
		client: client,
		names:  newNameResolver(client, names),
		submit: goredis.NewScript(string(source)),
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b, nil
}

// Submit records a score and returns where it puts the user.
func (b *Board) Submit(ctx context.Context, userID int64, game games.Slug, points int, at time.Time) (Placement, error) {
	if at.IsZero() {
		at = b.now()
	}

	scope := Game(game)
	keys := make([]string, 0, 2*len(Periods))
	args := make([]any, 0, 2+len(Periods))

	for _, period := range Periods {
		keys = append(keys, scope.Key(period, at))
	}
	for _, period := range Periods {
		keys = append(keys, Global().Key(period, at))
	}

	args = append(args, member(userID), points)
	for _, period := range Periods {
		args = append(args, int64(period.TTL().Seconds()))
	}

	raw, err := b.submit.Run(ctx, b.client, keys, args...).Slice()
	if err != nil {
		return Placement{}, fmt.Errorf("run submit script: %w", err)
	}

	const fieldsPerPeriod = 5
	if len(raw) != len(Periods)*fieldsPerPeriod {
		return Placement{}, fmt.Errorf("submit script returned %d values, want %d",
			len(raw), len(Periods)*fieldsPerPeriod)
	}

	placement := Placement{
		Improved: map[Period]bool{},
		Game:     map[Period]Standing{},
		Global:   map[Period]Standing{},
	}

	for i, period := range Periods {
		values, err := integers(raw[i*fieldsPerPeriod : (i+1)*fieldsPerPeriod])
		if err != nil {
			return Placement{}, err
		}

		placement.Improved[period] = values[0] == 1
		placement.Game[period] = Standing{Points: int(values[1]), Rank: values[2]}
		placement.Global[period] = Standing{Points: int(values[3]), Rank: values[4]}
	}
	return placement, nil
}

func integers(raw []any) ([]int64, error) {
	out := make([]int64, len(raw))

	for i, value := range raw {
		number, ok := value.(int64)
		if !ok {
			return nil, fmt.Errorf("submit script returned %T where a number was expected", value)
		}
		out[i] = number
	}
	return out, nil
}

// Size returns how many players are ranked on a board.
func (b *Board) Size(ctx context.Context, scope Scope, period Period) (int64, error) {
	total, err := b.client.ZCard(ctx, scope.Key(period, b.now())).Result()
	if err != nil {
		return 0, fmt.Errorf("count leaderboard: %w", err)
	}
	return total, nil
}

// Page reads a slice of a board, highest scores first.
func (b *Board) Page(ctx context.Context, scope Scope, period Period, offset, limit int) (Page, error) {
	if limit <= 0 || limit > MaxPageSize {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	key := scope.Key(period, b.now())

	total, err := b.Size(ctx, scope, period)
	if err != nil {
		return Page{}, err
	}

	rows, err := b.client.ZRevRangeWithScores(ctx, key, int64(offset), int64(offset+limit-1)).Result()
	if err != nil {
		return Page{}, fmt.Errorf("read leaderboard page: %w", err)
	}

	page := Page{
		Scope:   scope.Name(),
		Period:  period,
		Total:   total,
		Offset:  offset,
		Entries: []Entry{},
	}
	if len(rows) == 0 {
		return page, nil
	}

	// One ZCOUNT for the whole page. The rest of the ranks follow from the ordering: because the
	// page is contiguous and descending, a score's first occurrence is at its own position, so
	// its competition rank is that position plus one. Only the first row can begin part way
	// through a tie, which is why it alone needs asking.
	above, err := b.countAbove(ctx, key, rows[0].Score)
	if err != nil {
		return Page{}, err
	}

	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		id, err := parseMember(row.Member.(string))
		if err != nil {
			return Page{}, err
		}
		ids = append(ids, id)
	}

	usernames, err := b.names.resolve(ctx, ids)
	if err != nil {
		return Page{}, err
	}

	rank := above + 1
	for i, row := range rows {
		if i > 0 && row.Score != rows[i-1].Score {
			rank = int64(offset+i) + 1
		}

		page.Entries = append(page.Entries, Entry{
			Rank:     rank,
			UserID:   ids[i],
			Username: usernames[ids[i]],
			Points:   int(row.Score),
		})
	}
	return page, nil
}

// Standing returns one user's position, with the players immediately around them.
func (b *Board) Standing(ctx context.Context, scope Scope, period Period, userID int64, neighbours int) (Standing, []Entry, error) {
	key := scope.Key(period, b.now())

	score, err := b.client.ZScore(ctx, key, member(userID)).Result()
	if err != nil {
		if errors.Is(err, goredis.Nil) {
			return Standing{}, nil, ErrUnranked
		}
		return Standing{}, nil, fmt.Errorf("read score: %w", err)
	}

	above, err := b.countAbove(ctx, key, score)
	if err != nil {
		return Standing{}, nil, err
	}

	standing := Standing{Points: int(score), Rank: above + 1}
	if neighbours <= 0 {
		return standing, nil, nil
	}

	// The index, not the rank: ranks are shared between tied players, so they cannot address a
	// position in the list.
	index, err := b.client.ZRevRank(ctx, key, member(userID)).Result()
	if err != nil {
		return standing, nil, fmt.Errorf("read position: %w", err)
	}

	from := max(index-int64(neighbours), 0)
	page, err := b.Page(ctx, scope, period, int(from), 2*neighbours+1)
	if err != nil {
		return standing, nil, err
	}
	return standing, page.Entries, nil
}

// countAbove is how many members score strictly higher, which is the competition rank minus one.
func (b *Board) countAbove(ctx context.Context, key string, score float64) (int64, error) {
	count, err := b.client.ZCount(ctx, key,
		"("+formatScore(score), "+inf").Result()
	if err != nil {
		return 0, fmt.Errorf("count higher scores: %w", err)
	}
	return count, nil
}

func formatScore(score float64) string {
	return fmt.Sprintf("%d", int64(score))
}
