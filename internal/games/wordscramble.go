package games

import (
	"embed"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	wordScrambleDuration = 60 * time.Second
	wordScrambleTarget   = 15
	wordScrambleMinLen   = 5
)

//go:embed words.txt
var wordFile embed.FS

type wordList struct {
	words []string
	valid map[string]bool
}

var loadWords = sync.OnceValues(func() (wordList, error) {
	raw, err := wordFile.ReadFile("words.txt")
	if err != nil {
		return wordList{}, fmt.Errorf("read word list: %w", err)
	}

	list := wordList{valid: map[string]bool{}}

	for line := range strings.Lines(string(raw)) {
		word := strings.ToLower(strings.TrimSpace(line))
		if len(word) < wordScrambleMinLen {
			continue
		}
		list.words = append(list.words, word)
		list.valid[word] = true
	}

	if len(list.words) == 0 {
		return wordList{}, fmt.Errorf("word list is empty")
	}
	return list, nil
})

type wordScrambleState struct {
	Seed     int64     `json:"seed"`
	Deadline time.Time `json:"deadline"`
	Index    int       `json:"index"`
	Solved   int       `json:"solved"`
	Skipped  int       `json:"skipped"`
}

type wordScrambleView struct {
	Scramble     string    `json:"scramble"`
	Index        int       `json:"index"`
	Solved       int       `json:"solved"`
	Skipped      int       `json:"skipped"`
	Deadline     time.Time `json:"deadline"`
	DurationSecs int       `json:"duration_seconds"`
}

type wordScrambleMove struct {
	Answer string `json:"answer"`
	Skip   bool   `json:"skip"`
}

// wordScrambleWordAt derives the nth word from the seed.
func wordScrambleWordAt(seed int64, index int) string {
	list, err := loadWords()
	if err != nil {
		return ""
	}
	return list.words[deriveIntn(seed, "scramble-word", index, len(list.words))]
}

// scrambleWord shuffles a word deterministically. A shuffle that happens to reproduce the
// original is rotated, so the player is never handed the answer.
func scrambleWord(word string, seed int64, index int) string {
	letters := []rune(word)
	label := "scramble:" + strconv.Itoa(index)

	for i := len(letters) - 1; i > 0; i-- {
		j := deriveIntn(seed, label, i, i+1)
		letters[i], letters[j] = letters[j], letters[i]
	}

	if string(letters) == word && len(letters) > 1 {
		letters[0], letters[1] = letters[1], letters[0]
	}
	return string(letters)
}

func sortedLetters(word string) string {
	letters := []rune(strings.ToLower(word))
	slices.Sort(letters)
	return string(letters)
}

type wordScrambleLogic struct{}

func (wordScrambleLogic) start(seed int64, now time.Time) wordScrambleState {
	return wordScrambleState{Seed: seed, Deadline: now.Add(wordScrambleDuration)}
}

func (wordScrambleLogic) deadline(s wordScrambleState) time.Time { return s.Deadline }

func (wordScrambleLogic) view(s wordScrambleState) any {
	word := wordScrambleWordAt(s.Seed, s.Index)

	return wordScrambleView{
		Scramble:     scrambleWord(word, s.Seed, s.Index),
		Index:        s.Index,
		Solved:       s.Solved,
		Skipped:      s.Skipped,
		Deadline:     s.Deadline,
		DurationSecs: int(wordScrambleDuration.Seconds()),
	}
}

func (wordScrambleLogic) move(s wordScrambleState, raw json.RawMessage, now time.Time) (wordScrambleState, Outcome, error) {
	if now.After(s.Deadline) {
		return s, Outcome{}, ErrDeadlinePassed
	}

	move, err := decodeMove[wordScrambleMove](raw)
	if err != nil {
		return s, Outcome{}, err
	}

	if move.Skip {
		s.Skipped++
		s.Index++
		return s, Outcome{}, nil
	}

	answer := strings.ToLower(strings.TrimSpace(move.Answer))
	if answer == "" {
		return s, Outcome{}, fmt.Errorf("%w: an answer or skip is required", ErrInvalidMove)
	}

	list, err := loadWords()
	if err != nil {
		return s, Outcome{}, err
	}

	// Any real word built from exactly these letters counts, not only the word that happened
	// to be scrambled. A scramble can have more than one valid solution, and refusing the
	// others would mark a correct answer wrong.
	target := wordScrambleWordAt(s.Seed, s.Index)
	if !list.valid[answer] || sortedLetters(answer) != sortedLetters(target) {
		return s, Outcome{Correct: false}, nil
	}

	s.Solved++
	s.Index++
	return s, Outcome{Correct: true}, nil
}

func (wordScrambleLogic) raw(s wordScrambleState) float64 { return float64(s.Solved) }

func wordScrambleDefinition() Definition {
	return Definition{
		Slug:    WordScramble,
		Name:    "Word Scramble",
		Summary: "Unscramble as many words as you can in sixty seconds.",
		Rules: []string{
			"You have 60 seconds from the moment the session starts.",
			"Each puzzle is a real word with its letters shuffled.",
			"Any real word using exactly those letters is accepted, not just the original.",
			`Send {"skip":true} to move on without answering.`,
			"Your score is the number of words solved before the clock runs out.",
		},
		Metric:        "words solved",
		LowerIsBetter: false,
		Duration:      wordScrambleDuration,
		engine:        engineFor[wordScrambleState](wordScrambleLogic{}),
		points: func(raw float64) float64 {
			return scale(raw, 0, wordScrambleTarget)
		},
	}
}
