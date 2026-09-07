package games

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

const (
	memorySymbols  = 4
	memoryMaxLevel = 20
	memoryTarget   = 12
)

type memoryState struct {
	Seed      int64 `json:"seed"`
	Level     int   `json:"level"`
	Completed int   `json:"completed"`
	Done      bool  `json:"done"`
}

type memoryView struct {
	Level     int   `json:"level"`
	Sequence  []int `json:"sequence"`
	Symbols   int   `json:"symbols"`
	Completed int   `json:"completed"`
	Done      bool  `json:"done"`
}

type memoryMove struct {
	Answer []int `json:"answer"`
}

type memoryLogic struct{}

func (memoryLogic) start(seed int64, _ time.Time) memoryState {
	return memoryState{Seed: seed, Level: 1}
}

func (memoryLogic) deadline(memoryState) time.Time { return time.Time{} }

// memorySequence derives the sequence for a level. Level n is level n-1 plus one symbol, so the
// sequence a player has already learned never changes under them.
func memorySequence(seed int64, level int) []int {
	sequence := make([]int, level)
	for i := range sequence {
		sequence[i] = deriveIntn(seed, "memory", i, memorySymbols) + 1
	}
	return sequence
}

func (memoryLogic) view(s memoryState) any {
	view := memoryView{
		Level:     s.Level,
		Symbols:   memorySymbols,
		Completed: s.Completed,
		Done:      s.Done,
	}
	if !s.Done {
		view.Sequence = memorySequence(s.Seed, s.Level)
	}
	return view
}

func (memoryLogic) move(s memoryState, raw json.RawMessage, _ time.Time) (memoryState, Outcome, error) {
	if s.Done {
		return s, Outcome{}, ErrAlreadyDone
	}

	move, err := decodeMove[memoryMove](raw)
	if err != nil {
		return s, Outcome{}, err
	}
	if len(move.Answer) == 0 {
		return s, Outcome{}, fmt.Errorf("%w: an answer sequence is required", ErrInvalidMove)
	}

	if !slices.Equal(move.Answer, memorySequence(s.Seed, s.Level)) {
		s.Done = true
		return s, Outcome{Done: true}, nil
	}

	s.Completed = s.Level
	s.Level++

	if s.Level > memoryMaxLevel {
		s.Done = true
		return s, Outcome{Correct: true, Done: true}, nil
	}
	return s, Outcome{Correct: true}, nil
}

func (memoryLogic) raw(s memoryState) float64 { return float64(s.Completed) }

func memoryDefinition() Definition {
	return Definition{
		Slug:    Memory,
		Name:    "Memory Sequence",
		Summary: "Repeat a growing sequence of symbols. One mistake ends the run.",
		Rules: []string{
			"You are shown a sequence of symbols, numbered 1 to 4.",
			"Send it back in the same order.",
			"Every correct answer adds one symbol to the sequence.",
			"A single mistake ends the run.",
			"Your score is the length of the longest sequence you repeated correctly.",
		},
		Metric:        "sequence length",
		LowerIsBetter: false,
		engine:        engineFor[memoryState](memoryLogic{}),
		points: func(raw float64) float64 {
			return scale(raw, 0, memoryTarget)
		},
	}
}
