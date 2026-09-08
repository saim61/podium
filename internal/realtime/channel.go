// Package realtime pushes leaderboard changes to connected clients.
//
// What travels over Redis Pub/Sub is a notification, not a payload: "the memory daily board
// changed". Each instance then reads the current top N from Redis and sends that snapshot to its
// own subscribers. Publishing the rows themselves would mean every instance forwarding data most
// of them have no subscriber for, and clients applying diffs they could get wrong. A snapshot is
// authoritative by construction - a client that misses one is corrected by the next.
package realtime

import (
	"fmt"
	"strings"

	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/leaderboard"
)

// UpdateChannel is the single Redis Pub/Sub channel every instance listens on.
const UpdateChannel = "rt:leaderboard"

// Channel names one board a client can subscribe to, as "{scope}:{period}".
type Channel struct {
	Scope  leaderboard.Scope
	Period leaderboard.Period
}

// GlobalScopeName is how the cross-game board is addressed in a channel name.
const GlobalScopeName = "global"

// String renders the channel as clients and Pub/Sub messages spell it.
func (c Channel) String() string {
	return c.Scope.Name() + ":" + string(c.Period)
}

// ParseChannel reads a channel name, rejecting anything that does not name a real board.
func ParseChannel(raw string, registry *games.Registry) (Channel, error) {
	scopeName, periodName, found := strings.Cut(raw, ":")
	if !found || scopeName == "" || periodName == "" {
		return Channel{}, fmt.Errorf("channel %q must look like scope:period", raw)
	}

	// ParsePeriod treats an empty string as all-time, which is right for an omitted query
	// parameter and wrong here: "global:" is a malformed channel, not a request for all-time.
	period, err := leaderboard.ParsePeriod(periodName)
	if err != nil {
		return Channel{}, fmt.Errorf("channel %q: %w", raw, err)
	}

	if scopeName == GlobalScopeName {
		return Channel{Scope: leaderboard.Global(), Period: period}, nil
	}

	// Validated against the registry so a client cannot make the hub poll arbitrary keys.
	if _, err := registry.Get(games.Slug(scopeName)); err != nil {
		return Channel{}, fmt.Errorf("channel %q: %w", raw, err)
	}
	return Channel{Scope: leaderboard.Game(games.Slug(scopeName)), Period: period}, nil
}
