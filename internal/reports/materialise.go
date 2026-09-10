package reports

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/leaderboard"
	"github.com/saim61/podium/internal/store/db"
)

// MaterialiseReport says what a materialisation pass wrote.
type MaterialiseReport struct {
	Written int
	Skipped int
}

// snapshotSize is how many players a materialised window keeps.
//
// A report names leading players, so the tail is not worth storing: one row per closed window
// per board is already 24 rows a day, and keeping thousands of entries each would turn a
// reporting convenience into the biggest table in the database.
const snapshotSize = MaxLimit

// MaterialiseClosedWindows stores the leading players of every window that has just ended.
//
// Run from the worker, so an old report is one indexed row rather than an aggregation over every
// score ever recorded. It is idempotent: a window already stored is written again with the same
// content, which also means a late score that arrived after the first pass gets picked up.
func (s *Service) MaterialiseClosedWindows(ctx context.Context, at time.Time) (MaterialiseReport, error) {
	if at.IsZero() {
		at = s.now()
	}

	scopes := []leaderboard.Scope{leaderboard.Global()}
	for _, slug := range s.registry.Slugs() {
		scopes = append(scopes, leaderboard.Game(slug))
	}

	report := MaterialiseReport{}

	for _, period := range leaderboard.Periods {
		// All-time never closes, so there is no window to freeze.
		if period == leaderboard.AllTime {
			continue
		}

		// The window before the current one: the most recent that can no longer change much.
		from, _ := period.Window(at)
		previous := from.Add(-time.Nanosecond)

		for _, scope := range scopes {
			written, err := s.materialise(ctx, scope, period, previous)
			if err != nil {
				return report, err
			}
			if written {
				report.Written++
			} else {
				report.Skipped++
			}
		}
	}
	return report, nil
}

// materialise stores one board's window, reporting whether anything was written. A window with
// no players is skipped rather than stored as an empty row.
func (s *Service) materialise(ctx context.Context, scope leaderboard.Scope, period leaderboard.Period, at time.Time) (bool, error) {
	from, until := period.Window(at)

	rows, err := s.aggregate(ctx, Request{Scope: scope, Period: period, Limit: snapshotSize}, from, until)
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}

	entries, err := s.decorate(ctx, rows)
	if err != nil {
		return false, err
	}

	// Stored by id rather than by name, so a later rename shows through on the frozen report.
	stored := make([]snapshotEntry, 0, len(entries))
	for _, entry := range entries {
		stored = append(stored, snapshotEntry{
			Rank:   entry.Rank,
			UserID: entry.UserID,
			Points: entry.Points,
		})
	}

	encoded, err := json.Marshal(stored)
	if err != nil {
		return false, fmt.Errorf("encode leaderboard snapshot: %w", err)
	}

	if _, err := s.queries.UpsertLeaderboardSnapshot(ctx, db.UpsertLeaderboardSnapshotParams{
		Scope:       scope.Name(),
		Period:      string(period),
		WindowFrom:  from,
		WindowUntil: until,
		PlayerCount: int32(len(rows)),
		Entries:     encoded,
	}); err != nil {
		return false, fmt.Errorf("store leaderboard snapshot: %w", err)
	}
	return true, nil
}

// Games returns the registry, so a caller can validate a scope before asking for a report.
func (s *Service) Games() *games.Registry { return s.registry }
