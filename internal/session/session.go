// Package session runs game sessions: it owns their lifecycle, persistence and scoring, and is
// the only place a score is ever written.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/leaderboard"
	"github.com/saim61/podium/internal/platform/observability"
	"github.com/saim61/podium/internal/store/db"
)

// Failures a caller has to distinguish.
var (
	ErrNotFound = errors.New("session not found")
	ErrFinished = errors.New("session has already ended")
)

// Status values a session can hold.
const (
	StatusActive    = "active"
	StatusFinished  = "finished"
	StatusAbandoned = "abandoned"
)

// Session is a session as a caller sees it. It carries the view, never the state.
type Session struct {
	ID         uuid.UUID
	Game       games.Slug
	Status     string
	View       games.View
	Moves      int
	StartedAt  time.Time
	DeadlineAt *time.Time
	Score      *Score
	Placement  *leaderboard.Placement
}

// Score is what a finished session earned.
type Score struct {
	EventID    int64
	Game       games.Slug
	Metric     string
	Raw        float64
	Points     int
	AchievedAt time.Time
}

// Projector records a score on the leaderboards. Implemented by the leaderboard package.
type Projector interface {
	Submit(ctx context.Context, userID int64, game games.Slug, points int, at time.Time) (leaderboard.Placement, error)
}

// Service is the session lifecycle.
type Service struct {
	pool           *pgxpool.Pool
	queries        *db.Queries
	registry       *games.Registry
	projector      Projector
	metrics        *observability.Metrics
	projectTimeout time.Duration
	now            func() time.Time
	sleep          func(ctx context.Context, until time.Time) error
}

// Option adjusts a Service at construction.
type Option func(*Service)

// WithClock replaces the time source.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithProjector attaches the leaderboards a finished score is published to.
func WithProjector(p Projector) Option {
	return func(s *Service) { s.projector = p }
}

// WithMetrics attaches the collectors session activity is counted into.
func WithMetrics(m *observability.Metrics) Option {
	return func(s *Service) { s.metrics = m }
}

// WithProjectTimeout bounds how long the inline projection may take. A Redis outage must cost a
// player milliseconds, not the client's whole request: the score is already committed, and the
// projector sweeps whatever the deadline cut short.
func WithProjectTimeout(d time.Duration) Option {
	return func(s *Service) { s.projectTimeout = d }
}

// WithSleeper replaces the delay used to hold a response back. Tests use it to record what the
// engine asked for instead of waiting for it.
func WithSleeper(sleep func(ctx context.Context, until time.Time) error) Option {
	return func(s *Service) { s.sleep = sleep }
}

// NewService builds the session service.
func NewService(pool *pgxpool.Pool, registry *games.Registry, opts ...Option) *Service {
	s := &Service{
		pool:           pool,
		queries:        db.New(pool),
		registry:       registry,
		now:            time.Now,
		sleep:          sleepUntil,
		projectTimeout: 500 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func sleepUntil(ctx context.Context, until time.Time) error {
	delay := time.Until(until)
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Registry returns the games on offer.
func (s *Service) Registry() *games.Registry { return s.registry }

// Start opens a session. Any session the user already had open for the same game is abandoned,
// which bounds a user to one live session per game and stops sessions accumulating.
func (s *Service) Start(ctx context.Context, userID int64, slug games.Slug) (Session, error) {
	definition, err := s.registry.Get(slug)
	if err != nil {
		return Session{}, err
	}

	seed, err := games.NewSeed()
	if err != nil {
		return Session{}, err
	}

	state, view, deadline, err := definition.Engine().Start(seed, s.now())
	if err != nil {
		return Session{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Session{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)

	if _, err := q.AbandonActiveSessions(ctx, db.AbandonActiveSessionsParams{
		UserID: userID,
		Game:   string(slug),
	}); err != nil {
		return Session{}, fmt.Errorf("abandon previous sessions: %w", err)
	}

	var deadlineAt *time.Time
	if !deadline.IsZero() {
		deadlineAt = &deadline
	}

	row, err := q.CreateGameSession(ctx, db.CreateGameSessionParams{
		UserID:     userID,
		Game:       string(slug),
		Seed:       seed,
		State:      state,
		DeadlineAt: deadlineAt,
	})
	if err != nil {
		return Session{}, fmt.Errorf("create session: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Session{}, fmt.Errorf("commit session: %w", err)
	}

	if s.metrics != nil {
		s.metrics.SessionsStarted.WithLabelValues(string(slug)).Inc()
	}
	return sessionFrom(row, view, nil), nil
}

// Get returns a session the user owns.
func (s *Service) Get(ctx context.Context, userID int64, id uuid.UUID) (Session, error) {
	row, err := s.queries.GetGameSession(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrNotFound
		}
		return Session{}, fmt.Errorf("load session: %w", err)
	}
	if row.UserID != userID {
		// Not Forbidden: a distinct status for somebody else's session would confirm that the
		// id exists, which is a probe for other people's sessions.
		return Session{}, ErrNotFound
	}

	definition, err := s.registry.Get(games.Slug(row.Game))
	if err != nil {
		return Session{}, err
	}

	view, err := definition.Engine().View(row.State)
	if err != nil {
		return Session{}, err
	}

	score, err := s.scoreFor(ctx, s.queries, row)
	if err != nil {
		return Session{}, err
	}
	return sessionFrom(row, view, score), nil
}

// Move applies one move.
//
// The session row is locked FOR UPDATE for the duration, so two moves on the same session are
// serialised. Without that lock, concurrent moves would both read the same state and the second
// write would silently discard the first - which for math-sprint means answering twice and being
// credited once, and for memory means skipping a level.
func (s *Service) Move(ctx context.Context, userID int64, id uuid.UUID, move json.RawMessage) (Session, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Session{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)

	row, err := q.LockGameSession(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrNotFound
		}
		return Session{}, fmt.Errorf("lock session: %w", err)
	}
	if row.UserID != userID {
		return Session{}, ErrNotFound
	}
	if row.Status != StatusActive {
		return Session{}, ErrFinished
	}

	definition, err := s.registry.Get(games.Slug(row.Game))
	if err != nil {
		return Session{}, err
	}

	nextState, view, outcome, err := definition.Engine().Move(row.State, move, s.now())
	if err != nil {
		return Session{}, err
	}

	updated, err := q.SaveGameSessionState(ctx, db.SaveGameSessionStateParams{
		ID:    id,
		State: nextState,
	})
	if err != nil {
		return Session{}, fmt.Errorf("save session state: %w", err)
	}

	var score *Score
	if outcome.Done {
		finished, recorded, err := s.settle(ctx, q, updated, definition)
		if err != nil {
			return Session{}, err
		}
		updated, score = finished, recorded
	}

	if err := tx.Commit(ctx); err != nil {
		return Session{}, fmt.Errorf("commit move: %w", err)
	}

	// Held back only after the state is durable. Sleeping inside the transaction would keep the
	// row locked for up to three seconds and stall every other request touching it.
	if !outcome.HoldUntil.IsZero() {
		if err := s.sleep(ctx, outcome.HoldUntil); err != nil {
			return Session{}, err
		}
	}

	result := sessionFrom(updated, view, score)
	result.Placement = s.project(ctx, updated.UserID, score)
	return result, nil
}

// Finish ends a session and records its score. It is idempotent: finishing twice returns the
// score already stored rather than writing a second one.
func (s *Service) Finish(ctx context.Context, userID int64, id uuid.UUID) (Session, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Session{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)

	row, err := q.LockGameSession(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrNotFound
		}
		return Session{}, fmt.Errorf("lock session: %w", err)
	}
	if row.UserID != userID {
		return Session{}, ErrNotFound
	}

	definition, err := s.registry.Get(games.Slug(row.Game))
	if err != nil {
		return Session{}, err
	}

	view, err := definition.Engine().View(row.State)
	if err != nil {
		return Session{}, err
	}

	if row.Status != StatusActive {
		score, err := s.scoreFor(ctx, q, row)
		if err != nil {
			return Session{}, err
		}
		if score == nil {
			return Session{}, ErrFinished
		}
		return sessionFrom(row, view, score), nil
	}

	finished, score, err := s.settle(ctx, q, row, definition)
	if err != nil {
		return Session{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Session{}, fmt.Errorf("commit finish: %w", err)
	}

	result := sessionFrom(finished, view, score)
	result.Placement = s.project(ctx, finished.UserID, score)
	return result, nil
}

// project publishes a score to the leaderboards after it is durably recorded.
//
// A failure here is logged and swallowed. The score is already committed to Postgres, which is
// the source of truth; the sorted sets are a projection of it. Failing the request because a
// cache write failed would throw away a real score to protect a derived one - and the projector
// loop sweeps anything left with projected_at still null.
func (s *Service) project(ctx context.Context, userID int64, score *Score) *leaderboard.Placement {
	if s.projector == nil || score == nil {
		return nil
	}

	projectCtx, cancel := context.WithTimeout(ctx, s.projectTimeout)
	defer cancel()

	placement, err := s.projector.Submit(projectCtx, userID, score.Game, score.Points, score.AchievedAt)
	if err != nil {
		observability.Logger(ctx).Error("could not project score to leaderboards",
			slog.Int64("score_event_id", score.EventID),
			slog.Any("error", err))
		return nil
	}

	if err := s.queries.MarkScoreEventProjected(ctx, score.EventID); err != nil {
		observability.Logger(ctx).Error("could not mark score projected",
			slog.Int64("score_event_id", score.EventID),
			slog.Any("error", err))
	}
	return &placement
}

// settle marks a session finished and writes its score event.
func (s *Service) settle(
	ctx context.Context,
	q *db.Queries,
	row db.GameSession,
	definition games.Definition,
) (db.GameSession, *Score, error) {
	raw, err := definition.Engine().Raw(row.State)
	if err != nil {
		return row, nil, err
	}

	finished, err := q.FinishGameSession(ctx, row.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return row, nil, ErrFinished
		}
		return row, nil, fmt.Errorf("finish session: %w", err)
	}

	event, err := q.CreateScoreEvent(ctx, db.CreateScoreEventParams{
		UserID:    row.UserID,
		SessionID: row.ID,
		Game:      row.Game,
		Raw:       raw,
		Points:    int32(definition.Points(raw)),
	})
	if err != nil {
		// The unique index on session_id is the real guard against double scoring. Two
		// concurrent finishes both pass the status check only if one of them is working from a
		// stale read; the index turns that into an error instead of a duplicate score.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return finished, nil, ErrFinished
		}
		return finished, nil, fmt.Errorf("record score: %w", err)
	}

	if s.metrics != nil {
		s.metrics.ScoresRecorded.WithLabelValues(row.Game).Inc()
		s.metrics.ScorePoints.WithLabelValues(row.Game).Observe(float64(event.Points))
	}
	return finished, scoreFrom(event, definition), nil
}

func (s *Service) scoreFor(ctx context.Context, q *db.Queries, row db.GameSession) (*Score, error) {
	if row.Status == StatusActive {
		return nil, nil
	}

	event, err := q.GetScoreEventBySession(ctx, row.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("load score: %w", err)
	}

	definition, err := s.registry.Get(games.Slug(row.Game))
	if err != nil {
		return nil, err
	}
	return scoreFrom(event, definition), nil
}

func scoreFrom(event db.ScoreEvent, definition games.Definition) *Score {
	return &Score{
		EventID:    event.ID,
		Game:       games.Slug(event.Game),
		Metric:     definition.Metric,
		Raw:        event.Raw,
		Points:     int(event.Points),
		AchievedAt: event.AchievedAt,
	}
}

func sessionFrom(row db.GameSession, view games.View, score *Score) Session {
	return Session{
		ID:         row.ID,
		Game:       games.Slug(row.Game),
		Status:     row.Status,
		View:       view,
		Moves:      int(row.Moves),
		StartedAt:  row.StartedAt,
		DeadlineAt: row.DeadlineAt,
		Score:      score,
	}
}
