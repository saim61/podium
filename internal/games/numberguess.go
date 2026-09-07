package games

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	numberGuessRounds  = 3
	numberGuessCeiling = 100
	numberGuessBudget  = 7

	numberGuessWorst = numberGuessRounds * numberGuessBudget
	numberGuessBest  = 9
)

type numberGuessState struct {
	Secrets []int  `json:"secrets"`
	Round   int    `json:"round"`
	Low     int    `json:"low"`
	High    int    `json:"high"`
	Used    int    `json:"used"`
	Total   int    `json:"total"`
	Hint    string `json:"hint"`
	Done    bool   `json:"done"`
}

type numberGuessView struct {
	Round       int    `json:"round"`
	Rounds      int    `json:"rounds"`
	Low         int    `json:"low"`
	High        int    `json:"high"`
	GuessesUsed int    `json:"guesses_used"`
	GuessesLeft int    `json:"guesses_left"`
	Hint        string `json:"hint"`
	Solved      int    `json:"solved"`
	Done        bool   `json:"done"`
}

type numberGuessMove struct {
	Guess *int `json:"guess"`
}

type numberGuessLogic struct{}

func (numberGuessLogic) start(seed int64, _ time.Time) numberGuessState {
	secrets := make([]int, numberGuessRounds)
	for round := range secrets {
		secrets[round] = deriveRange(seed, "number-guess", round, 1, numberGuessCeiling)
	}

	return numberGuessState{
		Secrets: secrets,
		Low:     1,
		High:    numberGuessCeiling,
	}
}

func (numberGuessLogic) deadline(numberGuessState) time.Time { return time.Time{} }

func (numberGuessLogic) view(s numberGuessState) any {
	solved := s.Round
	if s.Done {
		solved = numberGuessRounds
	}

	return numberGuessView{
		Round:       min(s.Round+1, numberGuessRounds),
		Rounds:      numberGuessRounds,
		Low:         s.Low,
		High:        s.High,
		GuessesUsed: s.Used,
		GuessesLeft: numberGuessBudget - s.Used,
		Hint:        s.Hint,
		Solved:      solved,
		Done:        s.Done,
	}
}

func (numberGuessLogic) move(s numberGuessState, raw json.RawMessage, _ time.Time) (numberGuessState, Outcome, error) {
	if s.Done {
		return s, Outcome{}, ErrAlreadyDone
	}

	move, err := decodeMove[numberGuessMove](raw)
	if err != nil {
		return s, Outcome{}, err
	}
	if move.Guess == nil {
		return s, Outcome{}, fmt.Errorf("%w: a guess is required", ErrInvalidMove)
	}

	guess := *move.Guess
	if guess < 1 || guess > numberGuessCeiling {
		// Rejected without spending a guess: an out-of-range number is a client bug, not a
		// wrong answer, and charging for it would punish the wrong thing.
		return s, Outcome{}, fmt.Errorf("%w: guess must be between 1 and %d",
			ErrInvalidMove, numberGuessCeiling)
	}

	secret := s.Secrets[s.Round]
	s.Used++
	s.Total++

	switch {
	case guess == secret:
		s.Hint = "correct"
		return advanceNumberGuessRound(s), Outcome{Correct: true, Done: s.Round+1 >= numberGuessRounds}, nil

	case guess < secret:
		s.Hint = "higher"
		s.Low = max(s.Low, guess+1)

	default:
		s.Hint = "lower"
		s.High = min(s.High, guess-1)
	}

	if s.Used >= numberGuessBudget {
		s.Hint = "out of guesses"
		return advanceNumberGuessRound(s), Outcome{Done: s.Round+1 >= numberGuessRounds}, nil
	}
	return s, Outcome{}, nil
}

func advanceNumberGuessRound(s numberGuessState) numberGuessState {
	s.Round++
	s.Used = 0
	s.Low = 1
	s.High = numberGuessCeiling

	if s.Round >= numberGuessRounds {
		s.Done = true
	}
	return s
}

// raw is total guesses used, lower being better. Rounds never reached are charged the full
// budget, so quitting early cannot beat playing badly.
func (numberGuessLogic) raw(s numberGuessState) float64 {
	total := s.Total
	if !s.Done {
		total += (numberGuessRounds-s.Round)*numberGuessBudget - s.Used
	}
	return float64(total)
}

func numberGuessDefinition() Definition {
	return Definition{
		Slug:    NumberGuess,
		Name:    "Number Guess",
		Summary: "Find three hidden numbers between 1 and 100 in as few guesses as possible.",
		Rules: []string{
			"Each round hides a number between 1 and 100.",
			"After every guess you are told whether the target is higher or lower.",
			"You have 7 guesses per round, which is exactly enough to always win by halving the range.",
			"Three rounds. Your score is the total number of guesses used, so fewer is better.",
			"Unplayed rounds are charged the full 7 guesses.",
		},
		Metric:        "guesses",
		LowerIsBetter: true,
		engine:        engineFor[numberGuessState](numberGuessLogic{}),
		points: func(raw float64) float64 {
			return scale(raw, numberGuessWorst, numberGuessBest)
		},
	}
}
