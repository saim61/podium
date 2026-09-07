package games

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The whole anti-forgery argument rests on the client never receiving the answers. These tests
// check that claim directly against the JSON that actually goes over the wire.

func TestNumberGuessViewNeverContainsASecret(t *testing.T) {
	d, state, view := startGame(t, NumberGuess, 314159)

	var private numberGuessState
	require.NoError(t, json.Unmarshal(state, &private))
	require.Len(t, private.Secrets, numberGuessRounds)

	// Play a full round so the view carries hints, ranges and counters.
	for range numberGuessBudget {
		var outcome Outcome
		state, view, outcome = applyMove(t, d, state, map[string]int{"guess": 50}, testNow)
		if outcome.Done || outcome.Correct {
			break
		}
	}

	require.NotContains(t, string(view), `"secrets"`)
	for _, secret := range private.Secrets {
		requireNoStandaloneNumber(t, string(view), secret)
	}
}

func TestReactionViewNeverRevealsWhenTheSignalIsDue(t *testing.T) {
	d, state, view := startGame(t, Reaction, 271828)
	require.NotContains(t, string(view), "go_at")

	state, view, outcome := applyMove(t, d, state, map[string]string{"action": "arm"}, testNow)

	require.False(t, outcome.HoldUntil.IsZero(), "arming must hold the response back")
	require.NotContains(t, string(view), "go_at",
		"a client that knows the signal time can schedule a perfect tap")

	var private reactionState
	require.NoError(t, json.Unmarshal(state, &private))
	require.NotNil(t, private.GoAt, "the server must know, even though the client does not")
	require.Equal(t, outcome.HoldUntil, *private.GoAt)
}

func TestMathSprintViewNeverContainsTheAnswer(t *testing.T) {
	d, state, view := startGame(t, MathSprint, 161803)

	for index := range 8 {
		rendered := decodeView[mathSprintView](t, view)
		problem := mathSprintProblemAt(161803, index)

		require.Equal(t, problem.Question, rendered.Question)
		require.NotContains(t, string(view), `"answer"`)
		requireNoStandaloneNumber(t, string(view), problem.Answer)

		state, view, _ = applyMove(t, d, state, map[string]int{"answer": problem.Answer}, testNow)
	}
}

func TestWordScrambleViewNeverContainsThePlainWord(t *testing.T) {
	d, state, view := startGame(t, WordScramble, 141421)

	for index := range 6 {
		word := wordScrambleWordAt(141421, index)
		rendered := decodeView[wordScrambleView](t, view)

		require.NotEqual(t, word, rendered.Scramble, "the scramble must not be the word itself")
		require.NotContains(t, string(view), word)
		require.Equal(t, sortedLetters(word), sortedLetters(rendered.Scramble),
			"the scramble must be an anagram of the word")

		state, view, _ = applyMove(t, d, state, map[string]string{"answer": word}, testNow)
	}
}

// Memory is the honest exception, and it is worth an explicit test so nobody later assumes it
// behaves like the others. The game is to remember a sequence, so the sequence must be shown.
// Podium still owns the scoring, so a score cannot be forged - but the answer is, unavoidably,
// in the client's hands.
func TestMemoryMustRevealItsSequence(t *testing.T) {
	_, _, view := startGame(t, Memory, 173205)

	rendered := decodeView[memoryView](t, view)
	require.Len(t, rendered.Sequence, 1)
	require.Equal(t, memorySequence(173205, 1), rendered.Sequence)
}

// requireNoStandaloneNumber checks that a number does not appear in the JSON as its own value.
// A plain substring check would false-positive constantly: "7" appears inside "1757".
func requireNoStandaloneNumber(t *testing.T, document string, value int) {
	t.Helper()

	needle := strconv.Itoa(value)
	for _, pattern := range []string{":" + needle + ",", ":" + needle + "}", "[" + needle + ",", "," + needle + "]"} {
		require.False(t, strings.Contains(document, pattern),
			"%d appears as a value in %s", value, document)
	}
}
