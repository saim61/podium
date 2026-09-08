// Package leaderboard maintains Podium's rankings in Redis sorted sets.
//
// Redis is a derived read model here, not a database. Every entry can be rebuilt from the
// score_events table in Postgres, so losing Redis costs a rebuild and never data.
package leaderboard

import (
	"fmt"
	"strconv"
	"time"

	"github.com/saim61/podium/internal/games"
)

// Period is a leaderboard window.
type Period string

// The four windows every game is ranked over.
const (
	AllTime Period = "all-time"
	Daily   Period = "daily"
	Weekly  Period = "weekly"
	Monthly Period = "monthly"
)

// Periods is every period, in the order the Lua script expects its keys.
var Periods = []Period{AllTime, Daily, Weekly, Monthly}

// ParsePeriod reads a period from a query string, defaulting to all-time.
func ParsePeriod(raw string) (Period, error) {
	if raw == "" {
		return AllTime, nil
	}

	for _, p := range Periods {
		if string(p) == raw {
			return p, nil
		}
	}
	return "", fmt.Errorf("unknown period %q", raw)
}

// TTL is how long a period's keys are kept.
//
// Bounded so Redis memory does not grow without limit. Postgres keeps every score forever, so a
// report for a window older than this is served from there instead - see the cold path in the
// reports package.
func (p Period) TTL() time.Duration {
	switch p {
	case Daily:
		return 48 * time.Hour
	case Weekly:
		return 14 * 24 * time.Hour
	case Monthly:
		return 62 * 24 * time.Hour
	default:
		return 0
	}
}

// Window returns the half-open interval [from, until) a period covers at an instant. All-time
// returns two zero times.
func (p Period) Window(at time.Time) (time.Time, time.Time) {
	at = at.UTC()

	switch p {
	case Daily:
		from := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
		return from, from.AddDate(0, 0, 1)

	case Weekly:
		// ISO weeks start on Monday; Go's Weekday puts Sunday at zero.
		offset := (int(at.Weekday()) + 6) % 7
		from := time.Date(at.Year(), at.Month(), at.Day()-offset, 0, 0, 0, 0, time.UTC)
		return from, from.AddDate(0, 0, 7)

	case Monthly:
		from := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
		return from, from.AddDate(0, 1, 0)

	default:
		return time.Time{}, time.Time{}
	}
}

// bucket is the key suffix identifying which window an instant falls in.
func (p Period) bucket(at time.Time) string {
	at = at.UTC()

	switch p {
	case Daily:
		return "d:" + at.Format("2006-01-02")

	case Weekly:
		year, week := at.ISOWeek()
		return fmt.Sprintf("w:%d-W%02d", year, week)

	case Monthly:
		return "m:" + at.Format("2006-01")

	default:
		return "all"
	}
}

// Scope selects a single game's board, or the cross-game board when Game is empty.
type Scope struct {
	Game games.Slug
}

// Global is the cross-game scope.
func Global() Scope { return Scope{} }

// Game returns a single game's scope.
func Game(slug games.Slug) Scope { return Scope{Game: slug} }

// IsGlobal reports whether this is the cross-game board.
func (s Scope) IsGlobal() bool { return s.Game == "" }

// Name identifies the scope in a response.
func (s Scope) Name() string {
	if s.IsGlobal() {
		return "global"
	}
	return string(s.Game)
}

// Key returns the sorted set key for this scope and period.
func (s Scope) Key(period Period, at time.Time) string {
	if s.IsGlobal() {
		return "lb:global:" + period.bucket(at)
	}
	return "lb:g:" + string(s.Game) + ":" + period.bucket(at)
}

func member(userID int64) string { return strconv.FormatInt(userID, 10) }

func parseMember(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("leaderboard member %q is not a user id: %w", raw, err)
	}
	return id, nil
}
