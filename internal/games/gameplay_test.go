package games

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func secretsOf(t *testing.T, state State) []int {
	t.Helper()

	var private numberGuessState
	require.NoError(t, json.Unmarshal(state, &private))
	return private.Secrets
}

func TestNumberGuessPerfectPlayScoresWell(t *testing.T) {
	d, state, _ := startGame(t, NumberGuess, 555)
	secrets := secretsOf(t, state)

	for _, secret := range secrets {
		var outcome Outcome
		state, _, outcome = applyMove(t, d, state, map[string]int{"guess": secret}, testNow)
		require.True(t, outcome.Correct)
	}

	require.Equal(t, float64(numberGuessRounds), rawOf(t, d, state))
	require.Equal(t, MaxPoints, d.Points(rawOf(t, d, state)))
}

func TestNumberGuessGivesDirectionalHints(t *testing.T) {
	d, state, _ := startGame(t, NumberGuess, 777)
	secret := secretsOf(t, state)[0]

	require.Greater(t, secret, 1, "pick a seed whose first secret leaves room to guess low")

	_, view, _ := applyMove(t, d, state, map[string]int{"guess": 1}, testNow)

	rendered := decodeView[numberGuessView](t, view)
	require.Equal(t, "higher", rendered.Hint)
	require.Equal(t, 2, rendered.Low, "the hint should narrow the range")
	require.Equal(t, 1, rendered.GuessesUsed)
}

func TestNumberGuessRejectsOutOfRangeWithoutSpendingAGuess(t *testing.T) {
	d, state, _ := startGame(t, NumberGuess, 888)

	err := moveError(t, d, state, map[string]int{"guess": 0}, testNow)
	require.ErrorIs(t, err, ErrInvalidMove)

	err = moveError(t, d, state, map[string]int{"guess": numberGuessCeiling + 1}, testNow)
	require.ErrorIs(t, err, ErrInvalidMove)

	// The rejected guesses cost nothing, so the budget is untouched.
	_, view, _ := applyMove(t, d, state, map[string]int{"guess": 50}, testNow)
	require.Equal(t, 1, decodeView[numberGuessView](t, view).GuessesUsed)
}

func TestNumberGuessExhaustingTheBudgetMovesOn(t *testing.T) {
	d, state, _ := startGame(t, NumberGuess, 999)
	secret := secretsOf(t, state)[0]

	wrong := 1
	if secret == 1 {
		wrong = 2
	}

	var view View
	for range numberGuessBudget {
		state, view, _ = applyMove(t, d, state, map[string]int{"guess": wrong}, testNow)
	}

	rendered := decodeView[numberGuessView](t, view)
	require.Equal(t, 2, rendered.Round, "a spent budget should advance the round")
	require.Equal(t, 0, rendered.GuessesUsed)
}

// Abandoning a session must never beat playing it out, or the leaderboard rewards quitting.
func TestNumberGuessChargesRoundsNeverPlayed(t *testing.T) {
	d, state, _ := startGame(t, NumberGuess, 1234)

	require.Equal(t, float64(numberGuessWorst), rawOf(t, d, state),
		"an untouched session should already be the worst score")

	state, _, _ = applyMove(t, d, state, map[string]int{"guess": 50}, testNow)
	require.Equal(t, float64(numberGuessWorst), rawOf(t, d, state))
	require.Equal(t, 0, d.Points(rawOf(t, d, state)))
}

func TestNumberGuessRefusesMovesAfterTheGameEnds(t *testing.T) {
	d, state, _ := startGame(t, NumberGuess, 1111)

	for _, secret := range secretsOf(t, state) {
		state, _, _ = applyMove(t, d, state, map[string]int{"guess": secret}, testNow)
	}

	require.ErrorIs(t, moveError(t, d, state, map[string]int{"guess": 5}, testNow), ErrAlreadyDone)
}

func TestMemoryGrowsWithEveryCorrectAnswer(t *testing.T) {
	d, state, _ := startGame(t, Memory, 2024)

	for level := 1; level <= 6; level++ {
		answer := memorySequence(2024, level)

		var view View
		var outcome Outcome
		state, view, outcome = applyMove(t, d, state, map[string][]int{"answer": answer}, testNow)

		require.True(t, outcome.Correct, "level %d", level)

		rendered := decodeView[memoryView](t, view)
		require.Equal(t, level, rendered.Completed)
		require.Len(t, rendered.Sequence, level+1, "the next sequence should be one longer")
	}

	require.Equal(t, float64(6), rawOf(t, d, state))
}

func TestMemoryOneMistakeEndsTheRun(t *testing.T) {
	d, state, _ := startGame(t, Memory, 2025)

	state, _, _ = applyMove(t, d, state, map[string][]int{"answer": memorySequence(2025, 1)}, testNow)
	state, _, _ = applyMove(t, d, state, map[string][]int{"answer": memorySequence(2025, 2)}, testNow)

	wrong := memorySequence(2025, 3)
	wrong[0] = wrong[0]%memorySymbols + 1

	state, view, outcome := applyMove(t, d, state, map[string][]int{"answer": wrong}, testNow)

	require.True(t, outcome.Done)
	require.False(t, outcome.Correct)
	require.Equal(t, 2, decodeView[memoryView](t, view).Completed)
	require.Equal(t, float64(2), rawOf(t, d, state), "the score is the longest run completed")

	require.ErrorIs(t,
		moveError(t, d, state, map[string][]int{"answer": {1}}, testNow), ErrAlreadyDone)
}

func TestMemoryRejectsAnEmptyAnswer(t *testing.T) {
	d, state, _ := startGame(t, Memory, 2026)

	require.ErrorIs(t,
		moveError(t, d, state, map[string][]int{"answer": {}}, testNow), ErrInvalidMove)
}

func TestMathSprintCountsCorrectAnswers(t *testing.T) {
	const seed = 4321
	d, state, _ := startGame(t, MathSprint, seed)

	for index := range 5 {
		answer := mathSprintProblemAt(seed, index).Answer

		var outcome Outcome
		state, _, outcome = applyMove(t, d, state, map[string]int{"answer": answer}, testNow)
		require.True(t, outcome.Correct, "problem %d", index)
	}

	require.Equal(t, float64(5), rawOf(t, d, state))
	require.Positive(t, d.Points(rawOf(t, d, state)))
}

func TestMathSprintWrongAnswerAdvancesWithoutCredit(t *testing.T) {
	const seed = 4322
	d, state, _ := startGame(t, MathSprint, seed)

	wrong := mathSprintProblemAt(seed, 0).Answer + 1
	state, view, outcome := applyMove(t, d, state, map[string]int{"answer": wrong}, testNow)

	require.False(t, outcome.Correct)

	rendered := decodeView[mathSprintView](t, view)
	require.Equal(t, 1, rendered.Index, "a wrong answer should still move to the next question")
	require.Equal(t, 1, rendered.Wrong)
	require.Equal(t, 0, rendered.Correct)
	require.Equal(t, float64(0), rawOf(t, d, state))
}

// The deadline is the server's, checked on every move. A client that keeps playing past it
// simply stops scoring.
func TestMathSprintRefusesMovesAfterTheDeadline(t *testing.T) {
	const seed = 4323
	d, state, _ := startGame(t, MathSprint, seed)

	answer := mathSprintProblemAt(seed, 0).Answer
	late := testNow.Add(mathSprintDuration + time.Second)

	require.ErrorIs(t,
		moveError(t, d, state, map[string]int{"answer": answer}, late), ErrDeadlinePassed)

	// A move one instant before the deadline is still good.
	_, _, outcome := applyMove(t, d, state, map[string]int{"answer": answer},
		testNow.Add(mathSprintDuration-time.Millisecond))
	require.True(t, outcome.Correct)
}

func TestWordScrambleAcceptsTheTargetWord(t *testing.T) {
	const seed = 8080
	d, state, _ := startGame(t, WordScramble, seed)

	word := wordScrambleWordAt(seed, 0)
	state, view, outcome := applyMove(t, d, state, map[string]string{"answer": word}, testNow)

	require.True(t, outcome.Correct)

	rendered := decodeView[wordScrambleView](t, view)
	require.Equal(t, 1, rendered.Solved)
	require.Equal(t, 1, rendered.Index)
	require.Equal(t, float64(1), rawOf(t, d, state))
}

func TestWordScrambleIsCaseAndSpaceInsensitive(t *testing.T) {
	const seed = 8081
	d, state, _ := startGame(t, WordScramble, seed)

	word := wordScrambleWordAt(seed, 0)
	_, _, outcome := applyMove(t, d, state, map[string]string{"answer": "  " + upper(word) + " "}, testNow)

	require.True(t, outcome.Correct)
}

func upper(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'a' && r <= 'z' {
			out[i] = r - 32
		}
	}
	return string(out)
}

// A scramble can have more than one real solution. Accepting only the word that happened to be
// scrambled would mark a correct answer wrong.
func TestWordScrambleAcceptsAnyValidAnagram(t *testing.T) {
	list, err := loadWords()
	require.NoError(t, err)

	pairs := map[string][]string{}
	for _, word := range list.words {
		key := sortedLetters(word)
		pairs[key] = append(pairs[key], word)
	}

	var target, alternative string
	for _, group := range pairs {
		if len(group) >= 2 {
			target, alternative = group[0], group[1]
			break
		}
	}
	require.NotEmpty(t, target, "the word list needs at least one anagram pair for this test")

	index := indexOfWord(t, target)
	state, err := json.Marshal(wordScrambleState{
		Seed:     anagramSeed,
		Deadline: testNow.Add(wordScrambleDuration),
		Index:    index,
	})
	require.NoError(t, err)

	d := definition(t, WordScramble)
	_, _, outcome := applyMove(t, d, state, map[string]string{"answer": alternative}, testNow)

	require.True(t, outcome.Correct, "%q should be accepted for the scramble of %q",
		alternative, target)
}

const anagramSeed = 5150

func indexOfWord(t *testing.T, word string) int {
	t.Helper()

	for index := range 200_000 {
		if wordScrambleWordAt(anagramSeed, index) == word {
			return index
		}
	}
	t.Fatalf("no index produces %q", word)
	return 0
}

func TestWordScrambleRejectsNonWordsAndKeepsThePuzzle(t *testing.T) {
	const seed = 8082
	d, state, opening := startGame(t, WordScramble, seed)

	// The scramble itself has the right letters but is not a word.
	scramble := decodeView[wordScrambleView](t, opening).Scramble

	_, view, outcome := applyMove(t, d, state, map[string]string{"answer": scramble}, testNow)

	require.False(t, outcome.Correct)

	rendered := decodeView[wordScrambleView](t, view)
	require.Equal(t, 0, rendered.Solved)
	require.Equal(t, 0, rendered.Index, "a wrong answer should leave the same puzzle up")
}

func TestWordScrambleSkipMovesOn(t *testing.T) {
	const seed = 8083
	d, state, _ := startGame(t, WordScramble, seed)

	state, view, _ := applyMove(t, d, state, map[string]bool{"skip": true}, testNow)

	rendered := decodeView[wordScrambleView](t, view)
	require.Equal(t, 1, rendered.Index)
	require.Equal(t, 1, rendered.Skipped)
	require.Equal(t, float64(0), rawOf(t, d, state))
}

func TestWordScrambleRefusesMovesAfterTheDeadline(t *testing.T) {
	const seed = 8084
	d, state, _ := startGame(t, WordScramble, seed)

	late := testNow.Add(wordScrambleDuration + time.Second)
	word := wordScrambleWordAt(seed, 0)

	require.ErrorIs(t,
		moveError(t, d, state, map[string]string{"answer": word}, late), ErrDeadlinePassed)
}

func TestWordScrambleRequiresAnAnswerOrSkip(t *testing.T) {
	d, state, _ := startGame(t, WordScramble, 8085)

	require.ErrorIs(t,
		moveError(t, d, state, map[string]string{"answer": "   "}, testNow), ErrInvalidMove)
}

func TestReactionArmHoldsTheResponseUntilTheSignal(t *testing.T) {
	const seed = 6060
	d, state, _ := startGame(t, Reaction, seed)

	expectedDelay := deriveRange(seed, "reaction-delay", 0, reactionMinDelayMs, reactionMaxDelayMs)

	_, view, outcome := applyMove(t, d, state, map[string]string{"action": "arm"}, testNow)

	require.Equal(t, testNow.Add(time.Duration(expectedDelay)*time.Millisecond), outcome.HoldUntil)
	require.GreaterOrEqual(t, expectedDelay, reactionMinDelayMs)
	require.LessOrEqual(t, expectedDelay, reactionMaxDelayMs)
	require.True(t, decodeView[reactionView](t, view).Armed)
	require.Equal(t, "tap", decodeView[reactionView](t, view).Next)
}

func TestReactionRecordsTheDelayBetweenSignalAndTap(t *testing.T) {
	const seed = 6061
	d, state, _ := startGame(t, Reaction, seed)

	state, _, outcome := applyMove(t, d, state, map[string]string{"action": "arm"}, testNow)
	goAt := outcome.HoldUntil

	_, view, outcome := applyMove(t, d, state, map[string]string{"action": "tap"},
		goAt.Add(237*time.Millisecond))

	require.True(t, outcome.Correct)
	require.Equal(t, []int64{237}, decodeView[reactionView](t, view).Results)
}

// A tap faster than a person can react did not react to anything.
func TestReactionTreatsImpossiblyFastTapsAsFouls(t *testing.T) {
	const seed = 6062
	d, state, _ := startGame(t, Reaction, seed)

	state, _, outcome := applyMove(t, d, state, map[string]string{"action": "arm"}, testNow)
	goAt := outcome.HoldUntil

	_, view, outcome := applyMove(t, d, state, map[string]string{"action": "tap"},
		goAt.Add(10*time.Millisecond))

	require.False(t, outcome.Correct)

	rendered := decodeView[reactionView](t, view)
	require.Equal(t, 1, rendered.Fouls)
	require.Equal(t, []int64{reactionFoulMs}, rendered.Results)
}

func TestReactionTapBeforeTheSignalIsAFoul(t *testing.T) {
	const seed = 6063
	d, state, _ := startGame(t, Reaction, seed)

	state, _, _ = applyMove(t, d, state, map[string]string{"action": "arm"}, testNow)

	// Tapping immediately, without waiting for the held response to arrive.
	_, view, outcome := applyMove(t, d, state, map[string]string{"action": "tap"}, testNow)

	require.False(t, outcome.Correct)
	require.Equal(t, 1, decodeView[reactionView](t, view).Fouls)
}

func TestReactionRequiresArmingBeforeTapping(t *testing.T) {
	d, state, _ := startGame(t, Reaction, 6064)

	err := moveError(t, d, state, map[string]string{"action": "tap"}, testNow)
	require.ErrorIs(t, err, ErrInvalidMove)
	require.Contains(t, err.Error(), "arm the round")
}

func TestReactionRefusesToArmTwice(t *testing.T) {
	d, state, _ := startGame(t, Reaction, 6065)

	state, _, _ = applyMove(t, d, state, map[string]string{"action": "arm"}, testNow)

	require.ErrorIs(t,
		moveError(t, d, state, map[string]string{"action": "arm"}, testNow), ErrInvalidMove)
}

func TestReactionRejectsUnknownActions(t *testing.T) {
	d, state, _ := startGame(t, Reaction, 6066)

	require.ErrorIs(t,
		moveError(t, d, state, map[string]string{"action": "teleport"}, testNow), ErrInvalidMove)
}

func TestReactionScoresTheMeanOfFiveRounds(t *testing.T) {
	const seed = 6067
	d, state, _ := startGame(t, Reaction, seed)

	for round := range reactionRounds {
		var outcome Outcome
		state, _, outcome = applyMove(t, d, state, map[string]string{"action": "arm"}, testNow)

		state, _, outcome = applyMove(t, d, state, map[string]string{"action": "tap"},
			outcome.HoldUntil.Add(200*time.Millisecond))

		require.Equal(t, round == reactionRounds-1, outcome.Done)
	}

	require.Equal(t, float64(200), rawOf(t, d, state))
	require.ErrorIs(t,
		moveError(t, d, state, map[string]string{"action": "arm"}, testNow), ErrAlreadyDone)
}

func TestReactionChargesRoundsNeverPlayed(t *testing.T) {
	const seed = 6068
	d, state, _ := startGame(t, Reaction, seed)

	state, _, outcome := applyMove(t, d, state, map[string]string{"action": "arm"}, testNow)
	state, _, _ = applyMove(t, d, state, map[string]string{"action": "tap"},
		outcome.HoldUntil.Add(150*time.Millisecond))

	// One excellent round then quitting: mean of 150 and four 500ms penalties.
	expected := float64(150+4*reactionFoulMs) / reactionRounds
	require.Equal(t, expected, rawOf(t, d, state))

	require.Less(t, d.Points(rawOf(t, d, state)), MaxPoints/2,
		"one good round must not outscore a full game")
}
