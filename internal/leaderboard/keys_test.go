package leaderboard

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/saim61/podium/internal/games"
)

func at(iso string) time.Time {
	parsed, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		panic(err)
	}
	return parsed
}

func TestParsePeriod(t *testing.T) {
	for raw, want := range map[string]Period{
		"":         AllTime,
		"all-time": AllTime,
		"daily":    Daily,
		"weekly":   Weekly,
		"monthly":  Monthly,
	} {
		got, err := ParsePeriod(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, got, raw)
	}
}

func TestParsePeriodRejectsUnknown(t *testing.T) {
	for _, raw := range []string{"hourly", "ALL-TIME", "year", "1d"} {
		_, err := ParsePeriod(raw)
		require.Error(t, err, raw)
	}
}

func TestOnlyAllTimeIsKeptForever(t *testing.T) {
	require.Zero(t, AllTime.TTL())

	for _, period := range []Period{Daily, Weekly, Monthly} {
		require.Positive(t, period.TTL(), period)
	}

	// Each window is kept comfortably longer than it lasts, so a report for "yesterday" still
	// has its key.
	require.Greater(t, Daily.TTL(), 24*time.Hour)
	require.Greater(t, Weekly.TTL(), 7*24*time.Hour)
	require.Greater(t, Monthly.TTL(), 31*24*time.Hour)
}

func TestBucketNaming(t *testing.T) {
	instant := at("2026-09-07T13:45:00Z")

	require.Equal(t, "all", AllTime.bucket(instant))
	require.Equal(t, "d:2026-09-07", Daily.bucket(instant))
	require.Equal(t, "m:2026-09", Monthly.bucket(instant))
	require.Equal(t, "w:2026-W37", Weekly.bucket(instant))
}

func TestBucketsUseUTC(t *testing.T) {
	// Late evening in Auckland is still the previous day in UTC. Buckets must not depend on the
	// server's zone, or a leaderboard would roll over at a different moment per deployment.
	auckland, err := time.LoadLocation("Pacific/Auckland")
	if err != nil {
		t.Skip("tz database unavailable")
	}

	local := time.Date(2026, 9, 8, 9, 0, 0, 0, auckland)
	require.Equal(t, "d:2026-09-07", Daily.bucket(local))
}

func TestDailyWindow(t *testing.T) {
	from, until := Daily.Window(at("2026-09-07T13:45:00Z"))

	require.Equal(t, at("2026-09-07T00:00:00Z"), from)
	require.Equal(t, at("2026-09-08T00:00:00Z"), until)
}

// ISO weeks start on Monday. Go's Weekday numbers Sunday as zero, so the arithmetic is easy to
// get wrong by a day - which would put Sunday's scores in the wrong week.
func TestWeeklyWindowStartsOnMonday(t *testing.T) {
	for _, day := range []string{
		"2026-09-07T00:00:00Z", // Monday
		"2026-09-09T12:00:00Z", // Wednesday
		"2026-09-13T23:59:59Z", // Sunday
	} {
		from, until := Weekly.Window(at(day))

		require.Equal(t, at("2026-09-07T00:00:00Z"), from, day)
		require.Equal(t, at("2026-09-14T00:00:00Z"), until, day)
		require.Equal(t, time.Monday, from.Weekday(), day)
	}
}

func TestMonthlyWindow(t *testing.T) {
	from, until := Monthly.Window(at("2026-09-07T13:45:00Z"))

	require.Equal(t, at("2026-09-01T00:00:00Z"), from)
	require.Equal(t, at("2026-10-01T00:00:00Z"), until)
}

func TestMonthlyWindowAcrossYearEnd(t *testing.T) {
	from, until := Monthly.Window(at("2026-12-20T00:00:00Z"))

	require.Equal(t, at("2026-12-01T00:00:00Z"), from)
	require.Equal(t, at("2027-01-01T00:00:00Z"), until)
}

func TestAllTimeHasNoWindow(t *testing.T) {
	from, until := AllTime.Window(at("2026-09-07T13:45:00Z"))

	require.True(t, from.IsZero())
	require.True(t, until.IsZero())
}

func TestScopeKeys(t *testing.T) {
	instant := at("2026-09-07T13:45:00Z")

	require.Equal(t, "lb:g:memory:all", Game(games.Memory).Key(AllTime, instant))
	require.Equal(t, "lb:g:memory:d:2026-09-07", Game(games.Memory).Key(Daily, instant))
	require.Equal(t, "lb:global:all", Global().Key(AllTime, instant))
	require.Equal(t, "lb:global:w:2026-W37", Global().Key(Weekly, instant))
}

func TestScopeNaming(t *testing.T) {
	require.True(t, Global().IsGlobal())
	require.Equal(t, "global", Global().Name())

	require.False(t, Game(games.Reaction).IsGlobal())
	require.Equal(t, "reaction", Game(games.Reaction).Name())
}

// Two games must never share a key, or their scores would merge.
func TestEveryGameGetsItsOwnKey(t *testing.T) {
	instant := at("2026-09-07T13:45:00Z")
	seen := map[string]bool{}

	for _, slug := range games.NewRegistry().Slugs() {
		for _, period := range Periods {
			key := Game(slug).Key(period, instant)

			require.False(t, seen[key], "duplicate key %s", key)
			seen[key] = true
		}
	}
	require.Len(t, seen, 20, "five games across four periods")
}

func TestMemberRoundTrip(t *testing.T) {
	id, err := parseMember(member(4242))

	require.NoError(t, err)
	require.Equal(t, int64(4242), id)
}

func TestParseMemberRejectsNonNumeric(t *testing.T) {
	_, err := parseMember("saeem")

	require.Error(t, err)
}
