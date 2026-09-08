package leaderboard

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/saim61/podium/internal/games"
)

// Best is one player's best score in one game, as recorded in Postgres.
type Best struct {
	Game   games.Slug
	UserID int64
	Points int
}

// History is the authoritative score record a rebuild reads from.
//
// It supplies the clock as well as the scores. A score's achieved_at is stamped by Postgres, so
// the windows a rebuild buckets those scores into have to come from the same clock - otherwise
// any skew between the application host and the database puts a score near a boundary in one
// bucket when it is written and a different one when it is rebuilt.
type History interface {
	BestPoints(ctx context.Context, from, until time.Time) ([]Best, error)
	Now(ctx context.Context) (time.Time, error)
}

// RebuildReport says what a rebuild did.
type RebuildReport struct {
	Keys    int
	Entries int
	Periods []Period
}

// Rebuild recreates every leaderboard from Postgres.
//
// This is what makes Redis genuinely disposable rather than nominally so. Every sorted set here
// is a projection of score_events, and this function is the proof: flush Redis entirely and one
// call restores every board exactly.
//
// Only the current day, week and month buckets are rebuilt alongside all-time. Older buckets
// cannot be written back into a key that is named after the window it belongs to - and they do
// not need to be, since Postgres still holds the history and phase 7 serves old windows from
// there.
func (b *Board) Rebuild(ctx context.Context, history History) (RebuildReport, error) {
	now, err := history.Now(ctx)
	if err != nil {
		return RebuildReport{}, err
	}

	report := RebuildReport{}

	for _, period := range Periods {
		from, until := period.Window(now)

		best, err := history.BestPoints(ctx, from, until)
		if err != nil {
			return RebuildReport{}, err
		}

		keys, entries, err := b.rebuildPeriod(ctx, period, now, best)
		if err != nil {
			return RebuildReport{}, err
		}

		report.Keys += keys
		report.Entries += entries
		report.Periods = append(report.Periods, period)
	}
	return report, nil
}

// rebuildPeriod writes one period's boards, for every game plus the cross-game total.
func (b *Board) rebuildPeriod(ctx context.Context, period Period, now time.Time, best []Best) (int, int, error) {
	perGame := map[games.Slug]map[int64]int{}
	global := map[int64]int{}

	for _, entry := range best {
		if perGame[entry.Game] == nil {
			perGame[entry.Game] = map[int64]int{}
		}
		perGame[entry.Game][entry.UserID] = entry.Points

		// The cross-game board is the sum of per-game bests, recomputed here from scratch
		// rather than incrementally - which is exactly why a rebuild can repair drift that
		// incremental updates introduced.
		global[entry.UserID] += entry.Points
	}

	keys, entries := 0, 0

	for _, slug := range games.NewRegistry().Slugs() {
		written, err := b.replaceKey(ctx, Game(slug).Key(period, now), period, perGame[slug])
		if err != nil {
			return 0, 0, err
		}
		keys++
		entries += written
	}

	written, err := b.replaceKey(ctx, Global().Key(period, now), period, global)
	if err != nil {
		return 0, 0, err
	}
	return keys + 1, entries + written, nil
}

// replaceKey builds a sorted set under a temporary name and moves it into place.
//
// Built aside and RENAMEd rather than written in place, because RENAME is atomic: a reader either
// sees the whole old board or the whole new one. Deleting first and re-adding would leave a
// window in which the leaderboard is visibly empty or half filled.
func (b *Board) replaceKey(ctx context.Context, key string, period Period, scores map[int64]int) (int, error) {
	if len(scores) == 0 {
		// Nothing to rebuild means the board really is empty, so the old key must go.
		if err := b.client.Del(ctx, key).Err(); err != nil {
			return 0, fmt.Errorf("clear %s: %w", key, err)
		}
		return 0, nil
	}

	staging := key + ":rebuilding"
	if err := b.client.Del(ctx, staging).Err(); err != nil {
		return 0, fmt.Errorf("clear staging key %s: %w", staging, err)
	}

	members := make([]goredis.Z, 0, len(scores))
	for userID, points := range scores {
		members = append(members, goredis.Z{Score: float64(points), Member: member(userID)})
	}

	pipe := b.client.Pipeline()
	pipe.ZAdd(ctx, staging, members...)
	if ttl := period.TTL(); ttl > 0 {
		// Set on the staging key so the TTL survives the rename, which carries it across.
		pipe.Expire(ctx, staging, ttl)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("stage %s: %w", key, err)
	}

	if err := b.client.Rename(ctx, staging, key).Err(); err != nil {
		return 0, fmt.Errorf("swap %s into place: %w", key, err)
	}
	return len(members), nil
}
