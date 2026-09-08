// Package scores reads the authoritative score history in Postgres on behalf of the projector
// and the leaderboard rebuild.
package scores

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/leaderboard"
	"github.com/saim61/podium/internal/store/db"
)

// Event is one recorded score awaiting projection.
type Event struct {
	ID         int64
	UserID     int64
	Game       games.Slug
	Points     int
	AchievedAt time.Time
}

// Store reads and marks score history.
type Store struct {
	queries *db.Queries
}

// NewStore builds a store over a connection pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{queries: db.New(pool)}
}

// Unprojected returns scores that have not reached the leaderboards yet, oldest first.
func (s *Store) Unprojected(ctx context.Context, limit int) ([]Event, error) {
	rows, err := s.queries.ListUnprojectedScoreEvents(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("list unprojected scores: %w", err)
	}

	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		events = append(events, Event{
			ID:         row.ID,
			UserID:     row.UserID,
			Game:       games.Slug(row.Game),
			Points:     int(row.Points),
			AchievedAt: row.AchievedAt,
		})
	}
	return events, nil
}

// MarkProjected records that a score has reached the leaderboards.
func (s *Store) MarkProjected(ctx context.Context, id int64) error {
	if err := s.queries.MarkScoreEventProjected(ctx, id); err != nil {
		return fmt.Errorf("mark score projected: %w", err)
	}
	return nil
}

// PendingCount is how many scores are waiting to be projected. Exposed for metrics and for the
// readiness of a rebuild.
func (s *Store) PendingCount(ctx context.Context) (int64, error) {
	count, err := s.queries.CountUnprojectedScoreEvents(ctx)
	if err != nil {
		return 0, fmt.Errorf("count unprojected scores: %w", err)
	}
	return count, nil
}

// BestPoints returns each player's best score per game within [from, until). Zero times mean
// no bound, which is how the all-time boards are rebuilt.
//
// The aggregation happens in Postgres rather than by replaying every event in Go: a rebuild only
// needs one row per player per game, and pulling the full history to compute a maximum would
// scale with attempts rather than with players.
func (s *Store) BestPoints(ctx context.Context, from, until time.Time) ([]leaderboard.Best, error) {
	params := db.BestPointsPerUserAndGameParams{}
	if !from.IsZero() {
		params.FromTime = &from
	}
	if !until.IsZero() {
		params.UntilTime = &until
	}

	rows, err := s.queries.BestPointsPerUserAndGame(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("aggregate best points: %w", err)
	}

	best := make([]leaderboard.Best, 0, len(rows))
	for _, row := range rows {
		best = append(best, leaderboard.Best{
			Game:   games.Slug(row.Game),
			UserID: row.UserID,
			Points: int(row.Best),
		})
	}
	return best, nil
}

// Now returns the database's clock, which is the clock that stamps achieved_at.
func (s *Store) Now(ctx context.Context) (time.Time, error) {
	now, err := s.queries.Now(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("read database time: %w", err)
	}
	return now, nil
}

// DeleteExpiredRefreshTokens clears out refresh tokens nobody can use any more.
func (s *Store) DeleteExpiredRefreshTokens(ctx context.Context) (int64, error) {
	removed, err := s.queries.DeleteExpiredRefreshTokens(ctx)
	if err != nil {
		return 0, fmt.Errorf("delete expired refresh tokens: %w", err)
	}
	return removed, nil
}

// AbandonStaleSessions closes sessions nobody has touched for too long, so an abandoned game
// does not stay active forever.
func (s *Store) AbandonStaleSessions(ctx context.Context, olderThan time.Time) (int64, error) {
	abandoned, err := s.queries.AbandonStaleSessions(ctx, olderThan)
	if err != nil {
		return 0, fmt.Errorf("abandon stale sessions: %w", err)
	}
	return abandoned, nil
}
