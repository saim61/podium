package games

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	reactionRounds     = 5
	reactionMinDelayMs = 1200
	reactionMaxDelayMs = 3000

	// A tap this soon after the signal did not react to it. Either the client pre-fired, or it
	// is not a person. Charged as a foul rather than accepted as a superhuman score.
	reactionFloorMs = 80
	reactionFoulMs  = 500

	reactionWorstMs = 500
	reactionBestMs  = 180
)

type reactionState struct {
	Seed    int64      `json:"seed"`
	Round   int        `json:"round"`
	GoAt    *time.Time `json:"go_at"`
	Results []int64    `json:"results"`
	Fouls   int        `json:"fouls"`
	Done    bool       `json:"done"`
}

// reactionView deliberately omits GoAt. Telling the client when the signal is due would let it
// schedule a tap for that instant and post a perfect score without reacting to anything.
type reactionView struct {
	Round   int     `json:"round"`
	Rounds  int     `json:"rounds"`
	Armed   bool    `json:"armed"`
	Results []int64 `json:"results"`
	Fouls   int     `json:"fouls"`
	Next    string  `json:"next"`
	Done    bool    `json:"done"`
}

type reactionMove struct {
	Action string `json:"action"`
}

type reactionLogic struct{}

func (reactionLogic) start(seed int64, _ time.Time) reactionState {
	return reactionState{Seed: seed, Results: []int64{}}
}

func (reactionLogic) deadline(reactionState) time.Time { return time.Time{} }

func (reactionLogic) view(s reactionState) any {
	next := "arm"
	switch {
	case s.Done:
		next = "finish"
	case s.GoAt != nil:
		next = "tap"
	}

	return reactionView{
		Round:   min(s.Round+1, reactionRounds),
		Rounds:  reactionRounds,
		Armed:   s.GoAt != nil,
		Results: s.Results,
		Fouls:   s.Fouls,
		Next:    next,
		Done:    s.Done,
	}
}

func (reactionLogic) move(s reactionState, raw json.RawMessage, now time.Time) (reactionState, Outcome, error) {
	if s.Done {
		return s, Outcome{}, ErrAlreadyDone
	}

	move, err := decodeMove[reactionMove](raw)
	if err != nil {
		return s, Outcome{}, err
	}

	switch move.Action {
	case "arm":
		if s.GoAt != nil {
			return s, Outcome{}, fmt.Errorf("%w: this round is already armed", ErrInvalidMove)
		}

		delay := deriveRange(s.Seed, "reaction-delay", s.Round, reactionMinDelayMs, reactionMaxDelayMs)
		goAt := now.Add(time.Duration(delay) * time.Millisecond)
		s.GoAt = &goAt

		// The response to this move is the go signal, so it must not arrive early. The engine
		// cannot sleep, so it says when to release and the caller waits.
		return s, Outcome{HoldUntil: goAt}, nil

	case "tap":
		if s.GoAt == nil {
			return s, Outcome{}, fmt.Errorf("%w: arm the round before tapping", ErrInvalidMove)
		}

		elapsed := now.Sub(*s.GoAt).Milliseconds()

		foul := elapsed < reactionFloorMs
		if foul {
			s.Fouls++
			s.Results = append(s.Results, reactionFoulMs)
		} else {
			s.Results = append(s.Results, elapsed)
		}

		s.GoAt = nil
		s.Round++
		s.Done = s.Round >= reactionRounds

		return s, Outcome{Correct: !foul, Done: s.Done}, nil

	default:
		return s, Outcome{}, fmt.Errorf(`%w: action must be "arm" or "tap"`, ErrInvalidMove)
	}
}

// raw is the mean reaction across all five rounds. Rounds never played are charged the foul
// penalty, so stopping after one lucky round cannot beat completing the game.
func (reactionLogic) raw(s reactionState) float64 {
	var total int64
	for _, result := range s.Results {
		total += result
	}

	if missing := reactionRounds - len(s.Results); missing > 0 {
		total += int64(missing) * reactionFoulMs
	}
	return float64(total) / float64(reactionRounds)
}

func reactionDefinition() Definition {
	return Definition{
		Slug:    Reaction,
		Name:    "Reaction Time",
		Summary: "Five rounds of waiting for a signal and responding as fast as you can.",
		Rules: []string{
			`Send {"action":"arm"} to start a round. The response is held back for 1.2 to 3 seconds.`,
			`The moment that response arrives is the signal. Send {"action":"tap"} immediately.`,
			"Anything faster than 80ms is treated as a foul and charged 500ms.",
			"Five rounds. Your score is the mean, so lower is better.",
			"Rounds you never play are charged 500ms each.",
		},
		Metric:        "milliseconds",
		LowerIsBetter: true,
		engine:        engineFor[reactionState](reactionLogic{}),
		points: func(raw float64) float64 {
			return scale(raw, reactionWorstMs, reactionBestMs)
		},
	}
}
