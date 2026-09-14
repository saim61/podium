// Package projector keeps the Redis leaderboards in step with the scores recorded in Postgres.
package projector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/leaderboard"
	"github.com/saim61/podium/internal/platform/observability"
	"github.com/saim61/podium/internal/reports"
	"github.com/saim61/podium/internal/scores"
)

// Board is the leaderboard side of a projection.
type Board interface {
	Submit(ctx context.Context, userID int64, game games.Slug, points int, at time.Time) (leaderboard.Placement, error)
}

// Projector sweeps scores that never reached Redis and publishes them.
//
// The API projects a score inline as it is recorded, so this normally has nothing to do. It
// exists for when that inline attempt fails - Redis restarting, a network blip, the process dying
// between the commit and the publish. Without it those scores would be durably recorded and
// permanently invisible, which is the failure mode that quietly turns a leaderboard into a lie.
type Projector struct {
	store   *scores.Store
	board   Board
	cfg     config.Worker
	log     *slog.Logger
	metrics *observability.Metrics
}

// Option adjusts a Projector.
type Option func(*Projector)

// WithMetrics attaches the collectors projection progress is reported through.
func WithMetrics(m *observability.Metrics) Option {
	return func(p *Projector) { p.metrics = m }
}

// New builds a projector.
func New(store *scores.Store, board Board, cfg config.Worker, log *slog.Logger, opts ...Option) *Projector {
	p := &Projector{store: store, board: board, cfg: cfg, log: log}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Run sweeps on a ticker until the context is cancelled.
func (p *Projector) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.cfg.ProjectorInterval)
	defer ticker.Stop()

	p.log.Info("projector started",
		slog.String("interval", p.cfg.ProjectorInterval.String()),
		slog.Int("batch", p.cfg.ProjectorBatch))

	for {
		// Sweep immediately on start rather than waiting out the first tick, so a restart
		// after an outage catches up at once instead of leaving scores hidden for longer.
		projected, err := p.Sweep(ctx)
		switch {
		case errors.Is(err, context.Canceled):
			return nil
		case err != nil:
			// Logged, not returned: a failing sweep is exactly the condition this loop exists
			// to recover from, so it must keep trying rather than take the process down.
			p.log.Error("projection sweep failed", slog.Any("error", err))
		case projected > 0:
			p.log.Info("projected pending scores", slog.Int("count", projected))
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			p.log.Info("projector stopped")
			return nil
		}
	}
}

// Sweep projects one batch of pending scores and reports how many it published.
//
// Re-projecting a score that already reached Redis is harmless, which is what makes at-least-once
// delivery safe here: a game's board only ever accepts a strictly better score, so replaying an
// equal or worse one changes nothing, and the cross-game total moves by a delta that is then
// zero. The projection is idempotent because it is monotonic.
func (p *Projector) Sweep(ctx context.Context) (int, error) {
	pending, err := p.store.Unprojected(ctx, p.cfg.ProjectorBatch)
	if err != nil {
		return 0, err
	}

	// Reported before the work, so a growing backlog is visible while it is growing rather than
	// only once it has been cleared.
	if p.metrics != nil {
		p.metrics.ProjectionPending.Set(float64(len(pending)))
	}

	projected := 0
	for _, event := range pending {
		if _, err := p.board.Submit(ctx, event.UserID, event.Game, event.Points, event.AchievedAt); err != nil {
			return projected, fmt.Errorf("project score %d: %w", event.ID, err)
		}

		// Marked only after the publish succeeds. The other order would lose the score for good
		// if the publish then failed.
		if err := p.store.MarkProjected(ctx, event.ID); err != nil {
			return projected, err
		}
		projected++

		if p.metrics != nil {
			p.metrics.ProjectedTotal.Inc()
			p.metrics.ProjectionPending.Set(float64(len(pending) - projected))
		}
	}
	return projected, nil
}

// Materialiser freezes the leading players of a window that has closed.
type Materialiser interface {
	MaterialiseClosedWindows(ctx context.Context, at time.Time) (reports.MaterialiseReport, error)
}

// Housekeeper removes data nothing can use any more and freezes windows that have closed.
type Housekeeper struct {
	store       *scores.Store
	cfg         config.Worker
	log         *slog.Logger
	materialise Materialiser
}

// NewHousekeeper builds the housekeeping loop. materialise may be nil.
func NewHousekeeper(store *scores.Store, cfg config.Worker, log *slog.Logger, materialise Materialiser) *Housekeeper {
	return &Housekeeper{store: store, cfg: cfg, log: log, materialise: materialise}
}

// Run sweeps on a ticker until the context is cancelled.
func (h *Housekeeper) Run(ctx context.Context) error {
	ticker := time.NewTicker(h.cfg.HousekeepingInterval)
	defer ticker.Stop()

	h.log.Info("housekeeper started", slog.String("interval", h.cfg.HousekeepingInterval.String()))

	for {
		if err := h.Sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
			h.log.Error("housekeeping sweep failed", slog.Any("error", err))
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			h.log.Info("housekeeper stopped")
			return nil
		}
	}
}

// Sweep runs one round of housekeeping.
func (h *Housekeeper) Sweep(ctx context.Context) error {
	removed, err := h.store.DeleteExpiredRefreshTokens(ctx)
	if err != nil {
		return err
	}
	if removed > 0 {
		h.log.Info("removed expired refresh tokens", slog.Int64("count", removed))
	}

	abandoned, err := h.store.AbandonStaleSessions(ctx, time.Now().Add(-h.cfg.SessionMaxAge))
	if err != nil {
		return err
	}
	if abandoned > 0 {
		h.log.Info("abandoned stale sessions", slog.Int64("count", abandoned))
	}

	if h.materialise == nil {
		return nil
	}

	// Freezing a closed window turns an aggregation over the whole score history into a single
	// indexed row, which is what keeps an old report cheap once Redis has let the window go.
	frozen, err := h.materialise.MaterialiseClosedWindows(ctx, time.Now())
	if err != nil {
		return err
	}
	if frozen.Written > 0 {
		h.log.Info("materialised closed leaderboard windows",
			slog.Int("written", frozen.Written),
			slog.Int("skipped", frozen.Skipped))
	}
	return nil
}
