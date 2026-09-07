package games

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var testNow = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

func definition(t *testing.T, slug Slug) Definition {
	t.Helper()

	d, err := NewRegistry().Get(slug)
	require.NoError(t, err)
	return d
}

func startGame(t *testing.T, slug Slug, seed int64) (Definition, State, View) {
	t.Helper()

	d := definition(t, slug)
	state, view, _, err := d.Engine().Start(seed, testNow)
	require.NoError(t, err)
	return d, state, view
}

func applyMove(t *testing.T, d Definition, state State, move any, now time.Time) (State, View, Outcome) {
	t.Helper()

	encoded, err := json.Marshal(move)
	require.NoError(t, err)

	next, view, outcome, err := d.Engine().Move(state, encoded, now)
	require.NoError(t, err)
	return next, view, outcome
}

func moveError(t *testing.T, d Definition, state State, move any, now time.Time) error {
	t.Helper()

	encoded, err := json.Marshal(move)
	require.NoError(t, err)

	_, _, _, err = d.Engine().Move(state, encoded, now)
	require.Error(t, err)
	return err
}

func rawOf(t *testing.T, d Definition, state State) float64 {
	t.Helper()

	raw, err := d.Engine().Raw(state)
	require.NoError(t, err)
	return raw
}

func decodeView[T any](t *testing.T, view View) T {
	t.Helper()

	var out T
	require.NoError(t, json.Unmarshal(view, &out))
	return out
}

func TestRegistryHoldsAllFiveGames(t *testing.T) {
	r := NewRegistry()

	require.Len(t, r.All(), 5)
	require.ElementsMatch(t,
		[]Slug{Reaction, MathSprint, Memory, WordScramble, NumberGuess},
		r.Slugs())
}

func TestEveryGameIsFullyDescribed(t *testing.T) {
	for _, d := range NewRegistry().All() {
		t.Run(string(d.Slug), func(t *testing.T) {
			require.NotEmpty(t, d.Name)
			require.NotEmpty(t, d.Summary)
			require.NotEmpty(t, d.Rules)
			require.NotEmpty(t, d.Metric)
			require.NotNil(t, d.Engine())
			require.NotNil(t, d.points)
		})
	}
}

func TestUnknownGameIsRejected(t *testing.T) {
	_, err := NewRegistry().Get("pinball")

	require.ErrorIs(t, err, ErrNoSuchGame)
}

func TestPointsAreClampedToTheScale(t *testing.T) {
	for _, d := range NewRegistry().All() {
		t.Run(string(d.Slug), func(t *testing.T) {
			// Absurd values in both directions, whichever way the metric runs.
			require.GreaterOrEqual(t, d.Points(-1_000_000), 0)
			require.LessOrEqual(t, d.Points(-1_000_000), MaxPoints)
			require.GreaterOrEqual(t, d.Points(1_000_000), 0)
			require.LessOrEqual(t, d.Points(1_000_000), MaxPoints)
		})
	}
}

// Redis only ever stores points, so points must always be higher-is-better even for games whose
// own metric runs the other way. Without this the cross-game sorted set would rank the worst
// reaction times first.
func TestPointsAlwaysRunHigherIsBetter(t *testing.T) {
	// Realistic pairs inside each game's own scale, since values outside it clamp and would
	// compare equal.
	samples := map[Slug]struct{ worse, better float64 }{
		Reaction:     {worse: 450, better: 220},
		NumberGuess:  {worse: 19, better: 12},
		MathSprint:   {worse: 5, better: 22},
		Memory:       {worse: 3, better: 9},
		WordScramble: {worse: 3, better: 11},
	}

	for _, d := range NewRegistry().All() {
		t.Run(string(d.Slug), func(t *testing.T) {
			sample, ok := samples[d.Slug]
			require.True(t, ok, "add a sample pair for this game")

			require.Greater(t, d.Points(sample.better), d.Points(sample.worse),
				"a better raw metric must earn more points")
			require.Positive(t, d.Points(sample.better))
		})
	}
}

func TestEveryGameStartsCleanlyAndScoresZeroish(t *testing.T) {
	for _, d := range NewRegistry().All() {
		t.Run(string(d.Slug), func(t *testing.T) {
			state, view, deadline, err := d.Engine().Start(99, testNow)
			require.NoError(t, err)
			require.NotEmpty(t, state)
			require.NotEmpty(t, view)

			if d.Duration > 0 {
				require.Equal(t, testNow.Add(d.Duration), deadline)
			} else {
				require.True(t, deadline.IsZero())
			}

			raw, err := d.Engine().Raw(state)
			require.NoError(t, err)
			require.Equal(t, 0, d.Points(raw),
				"an untouched session must be worth nothing")
		})
	}
}

func TestStartIsDeterministicForASeed(t *testing.T) {
	for _, d := range NewRegistry().All() {
		t.Run(string(d.Slug), func(t *testing.T) {
			first, firstView, _, err := d.Engine().Start(4242, testNow)
			require.NoError(t, err)
			second, secondView, _, err := d.Engine().Start(4242, testNow)
			require.NoError(t, err)

			require.JSONEq(t, string(first), string(second))
			require.JSONEq(t, string(firstView), string(secondView))
		})
	}
}

// Games whose puzzle is necessarily visible - a question, a scramble, a sequence - must vary
// with the seed, or every session would pose the same challenge.
func TestSeedVariesTheVisiblePuzzle(t *testing.T) {
	for _, slug := range []Slug{MathSprint, WordScramble, Memory} {
		t.Run(string(slug), func(t *testing.T) {
			d := definition(t, slug)

			var views []string
			for seed := range int64(12) {
				_, view, _, err := d.Engine().Start(seed, testNow)
				require.NoError(t, err)
				views = append(views, string(view))
			}

			require.Greater(t, len(distinct(views)), 1,
				"the seed should decide the content of the game")
		})
	}
}

// Number guess is the opposite case and the clearest demonstration of the State/View split: the
// seed completely determines the game, yet two sessions with different seeds look identical from
// outside. The secrets are in State, which never leaves the server.
func TestNumberGuessSeedChangesStateButNotTheView(t *testing.T) {
	d := definition(t, NumberGuess)

	var states, views []string
	for seed := range int64(12) {
		state, view, _, err := d.Engine().Start(seed, testNow)
		require.NoError(t, err)
		states = append(states, string(state))
		views = append(views, string(view))
	}

	require.Greater(t, len(distinct(states)), 1, "the seed must pick different secrets")
	require.Len(t, distinct(views), 1, "the view must reveal nothing about them")
}

func distinct(values []string) []string {
	seen := map[string]bool{}
	var out []string

	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func TestMovesMustBeWellFormed(t *testing.T) {
	for _, d := range NewRegistry().All() {
		t.Run(string(d.Slug), func(t *testing.T) {
			state, _, _, err := d.Engine().Start(7, testNow)
			require.NoError(t, err)

			for name, raw := range map[string]string{
				"empty":         ``,
				"not an object": `42`,
				"unknown field": `{"definitely_not_a_field":1}`,
			} {
				_, _, _, err := d.Engine().Move(state, json.RawMessage(raw), testNow)
				require.Error(t, err, name)
			}
		})
	}
}

func TestCorruptStateIsReportedNotPanicked(t *testing.T) {
	for _, d := range NewRegistry().All() {
		t.Run(string(d.Slug), func(t *testing.T) {
			require.NotPanics(t, func() {
				_, err := d.Engine().Raw(json.RawMessage(`not json`))
				require.Error(t, err)
			})
		})
	}
}

func TestSeedsAreUnpredictable(t *testing.T) {
	seen := map[int64]bool{}

	for range 200 {
		seed, err := NewSeed()
		require.NoError(t, err)
		require.Positive(t, seed, "a negative seed would break derive's index arithmetic")
		require.False(t, seen[seed], "seeds must not repeat")
		seen[seed] = true
	}
}

func TestDeriveIsStableAndSpreadsAcrossTheRange(t *testing.T) {
	require.Equal(t, derive(1, "label", 0), derive(1, "label", 0))
	require.NotEqual(t, derive(1, "label", 0), derive(2, "label", 0))
	require.NotEqual(t, derive(1, "label", 0), derive(1, "other", 0))
	require.NotEqual(t, derive(1, "label", 0), derive(1, "label", 1))

	counts := map[int]int{}
	for i := range 600 {
		counts[deriveRange(9, "spread", i, 1, 6)]++
	}

	require.Len(t, counts, 6, "every value in the range should appear")
	for value, count := range counts {
		require.Greater(t, count, 40, "value %d appeared only %d times", value, count)
	}
}
