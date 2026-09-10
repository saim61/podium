// Package reports answers "who were the best players in this window", from whichever store can
// still answer it.
package reports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/leaderboard"
	"github.com/saim61/podium/internal/store/db"
	"github.com/saim61/podium/internal/user"
)

// Source says which store answered a report. Returned to the caller so the split is observable
// rather than a claim in a document.
type Source string

const (
	// SourceLive means Redis still holds the window's sorted set.
	SourceLive Source = "live"

	// SourceSnapshot means the window was materialised into Postgres when it closed.
	SourceSnapshot Source = "snapshot"

	// SourceHistory means it was computed from score_events on demand.
	SourceHistory Source = "history"
)

// MaxLimit caps how many players one report can name.
const MaxLimit = 100

// Report is a period's leading players.
type Report struct {
	Scope   string              `json:"scope"`
	Period  leaderboard.Period  `json:"period"`
	From    *time.Time          `json:"from,omitempty"`
	Until   *time.Time          `json:"until,omitempty"`
	Source  Source              `json:"source"`
	Players int                 `json:"players"`
	Entries []leaderboard.Entry `json:"entries"`
}

// Boards is the Redis side of a report.
type Boards interface {
	Exists(ctx context.Context, scope leaderboard.Scope, period leaderboard.Period, at time.Time) (bool, error)
	PageAt(ctx context.Context, scope leaderboard.Scope, period leaderboard.Period, at time.Time, offset, limit int) (leaderboard.Page, error)
}

// Service answers reports.
type Service struct {
	pool     *pgxpool.Pool
	queries  *db.Queries
	boards   Boards
	names    *user.Directory
	registry *games.Registry
	now      func() time.Time
}

// Option adjusts a Service.
type Option func(*Service)

// WithClock replaces the time source.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService builds the report service.
func NewService(pool *pgxpool.Pool, boards Boards, registry *games.Registry, opts ...Option) *Service {
	s := &Service{
		pool:     pool,
		queries:  db.New(pool),
		boards:   boards,
		names:    user.NewDirectory(pool),
		registry: registry,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Request asks for one report.
type Request struct {
	Scope  leaderboard.Scope
	Period leaderboard.Period
	At     time.Time
	Limit  int
}

// TopPlayers answers a report from the cheapest store that still holds the window.
//
// Three stores, tried in order of cost. Redis answers in log time but only keeps a window while
// its key lives. A closed window that was materialised is one row of jsonb. Anything older is
// aggregated from score_events, which is always correct and always the slowest - so it is the
// fallback, not the default.
func (s *Service) TopPlayers(ctx context.Context, request Request) (Report, error) {
	if request.At.IsZero() {
		request.At = s.now()
	}
	if request.Limit <= 0 || request.Limit > MaxLimit {
		request.Limit = 10
	}

	from, until := request.Period.Window(request.At)

	report := Report{
		Scope:   request.Scope.Name(),
		Period:  request.Period,
		Entries: []leaderboard.Entry{},
	}
	if !from.IsZero() {
		report.From, report.Until = &from, &until
	}

	live, err := s.boards.Exists(ctx, request.Scope, request.Period, request.At)
	if err != nil {
		return Report{}, err
	}
	if live {
		page, err := s.boards.PageAt(ctx, request.Scope, request.Period, request.At, 0, request.Limit)
		if err != nil {
			return Report{}, err
		}

		report.Source = SourceLive
		report.Players = int(page.Total)
		report.Entries = page.Entries
		return report, nil
	}

	stored, err := s.fromSnapshot(ctx, request, from)
	if err != nil {
		return Report{}, err
	}
	if stored != nil {
		report.Source = SourceSnapshot
		report.Players = stored.players
		report.Entries = stored.entries
		return report, nil
	}

	entries, players, err := s.fromHistory(ctx, request, from, until)
	if err != nil {
		return Report{}, err
	}

	report.Source = SourceHistory
	report.Players = players
	report.Entries = entries
	return report, nil
}

type storedSnapshot struct {
	players int
	entries []leaderboard.Entry
}

// snapshotEntry is the stored form of a frozen row.
//
// It keeps the user id, which leaderboard.Entry deliberately withholds from API responses. A
// snapshot that stored only names would freeze them: after a rename, a historical report would
// show a name nobody has any more, while the live and history paths both resolve the current
// one. Storing the id keeps all three consistent.
type snapshotEntry struct {
	Rank   int64 `json:"rank"`
	UserID int64 `json:"user_id"`
	Points int   `json:"points"`
}

func (s *Service) fromSnapshot(ctx context.Context, request Request, from time.Time) (*storedSnapshot, error) {
	if from.IsZero() {
		// All-time has no window to have closed, so there is nothing to have snapshotted.
		return nil, nil
	}

	row, err := s.queries.GetLeaderboardSnapshot(ctx, db.GetLeaderboardSnapshotParams{
		Scope:      request.Scope.Name(),
		Period:     string(request.Period),
		WindowFrom: from,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("load leaderboard snapshot: %w", err)
	}

	var stored []snapshotEntry
	if err := json.Unmarshal(row.Entries, &stored); err != nil {
		return nil, fmt.Errorf("decode leaderboard snapshot: %w", err)
	}

	if len(stored) > request.Limit {
		stored = stored[:request.Limit]
	}

	ids := make([]int64, 0, len(stored))
	for _, entry := range stored {
		ids = append(ids, entry.UserID)
	}

	names, err := s.names.Usernames(ctx, ids)
	if err != nil {
		return nil, err
	}

	entries := make([]leaderboard.Entry, 0, len(stored))
	for _, entry := range stored {
		entries = append(entries, leaderboard.Entry{
			Rank:     entry.Rank,
			UserID:   entry.UserID,
			Username: names[entry.UserID],
			Points:   entry.Points,
		})
	}
	return &storedSnapshot{players: int(row.PlayerCount), entries: entries}, nil
}

// scored is one row of the cold path, before names and ranks are attached.
type scored struct {
	userID int64
	points int
}

// fromHistory aggregates score_events.
//
// The aggregation deliberately mirrors what submit.lua does in Redis: a game board takes each
// player's best, and the cross-game board sums those bests. If the two disagreed, the same
// window would rank people differently depending only on how old it happened to be.
func (s *Service) fromHistory(ctx context.Context, request Request, from, until time.Time) ([]leaderboard.Entry, int, error) {
	rows, err := s.aggregate(ctx, request, from, until)
	if err != nil {
		return nil, 0, err
	}

	entries, err := s.decorate(ctx, rows)
	if err != nil {
		return nil, 0, err
	}
	return entries, len(rows), nil
}

func (s *Service) aggregate(ctx context.Context, request Request, from, until time.Time) ([]scored, error) {
	limit := int32(request.Limit)

	switch {
	case request.Scope.IsGlobal() && from.IsZero():
		rows, err := s.queries.TopPlayersGlobalAllTime(ctx, limit)
		if err != nil {
			return nil, fmt.Errorf("aggregate global all-time report: %w", err)
		}

		out := make([]scored, 0, len(rows))
		for _, row := range rows {
			out = append(out, scored{userID: row.UserID, points: int(row.Points)})
		}
		return out, nil

	case request.Scope.IsGlobal():
		rows, err := s.queries.TopPlayersGlobalInWindow(ctx, db.TopPlayersGlobalInWindowParams{
			AchievedAt:   from,
			AchievedAt_2: until,
			Limit:        limit,
		})
		if err != nil {
			return nil, fmt.Errorf("aggregate global report: %w", err)
		}

		out := make([]scored, 0, len(rows))
		for _, row := range rows {
			out = append(out, scored{userID: row.UserID, points: int(row.Points)})
		}
		return out, nil

	case from.IsZero():
		rows, err := s.queries.TopPlayersForGameAllTime(ctx, db.TopPlayersForGameAllTimeParams{
			Game:  string(request.Scope.Game),
			Limit: limit,
		})
		if err != nil {
			return nil, fmt.Errorf("aggregate game all-time report: %w", err)
		}

		out := make([]scored, 0, len(rows))
		for _, row := range rows {
			out = append(out, scored{userID: row.UserID, points: int(row.Points)})
		}
		return out, nil

	default:
		rows, err := s.queries.TopPlayersForGameInWindow(ctx, db.TopPlayersForGameInWindowParams{
			Game:         string(request.Scope.Game),
			AchievedAt:   from,
			AchievedAt_2: until,
			Limit:        limit,
		})
		if err != nil {
			return nil, fmt.Errorf("aggregate game report: %w", err)
		}

		out := make([]scored, 0, len(rows))
		for _, row := range rows {
			out = append(out, scored{userID: row.UserID, points: int(row.Points)})
		}
		return out, nil
	}
}

// decorate attaches usernames and competition ranks, matching what the live path returns so a
// caller cannot tell the two apart from the shape of the answer.
func (s *Service) decorate(ctx context.Context, rows []scored) ([]leaderboard.Entry, error) {
	entries := make([]leaderboard.Entry, 0, len(rows))
	if len(rows) == 0 {
		return entries, nil
	}

	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.userID)
	}

	names, err := s.names.Usernames(ctx, ids)
	if err != nil {
		return nil, err
	}

	rank := int64(1)
	for i, row := range rows {
		if i > 0 && row.points != rows[i-1].points {
			rank = int64(i) + 1
		}

		entries = append(entries, leaderboard.Entry{
			Rank:     rank,
			UserID:   row.userID,
			Username: names[row.userID],
			Points:   row.points,
		})
	}
	return entries, nil
}
