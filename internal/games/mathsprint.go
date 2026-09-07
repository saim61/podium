package games

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	mathSprintDuration = 30 * time.Second
	mathSprintTarget   = 30
)

type mathSprintState struct {
	Seed     int64     `json:"seed"`
	Deadline time.Time `json:"deadline"`
	Index    int       `json:"index"`
	Correct  int       `json:"correct"`
	Wrong    int       `json:"wrong"`
}

type mathSprintView struct {
	Question     string    `json:"question"`
	Index        int       `json:"index"`
	Correct      int       `json:"correct"`
	Wrong        int       `json:"wrong"`
	Deadline     time.Time `json:"deadline"`
	DurationSecs int       `json:"duration_seconds"`
}

type mathSprintMove struct {
	Answer *int `json:"answer"`
}

type mathSprintProblem struct {
	Question string
	Answer   int
}

// mathSprintProblemAt derives the nth problem. Numbers stay small enough to be mental
// arithmetic; the game is a speed test, not a calculation test.
func mathSprintProblemAt(seed int64, index int) mathSprintProblem {
	switch deriveIntn(seed, "math-op", index, 3) {
	case 0:
		a := deriveRange(seed, "math-a", index, 12, 99)
		b := deriveRange(seed, "math-b", index, 12, 99)
		return mathSprintProblem{fmt.Sprintf("%d + %d", a, b), a + b}

	case 1:
		a := deriveRange(seed, "math-a", index, 30, 99)
		b := deriveRange(seed, "math-b", index, 2, 29)
		return mathSprintProblem{fmt.Sprintf("%d - %d", a, b), a - b}

	default:
		a := deriveRange(seed, "math-a", index, 2, 12)
		b := deriveRange(seed, "math-b", index, 2, 12)
		return mathSprintProblem{fmt.Sprintf("%d x %d", a, b), a * b}
	}
}

type mathSprintLogic struct{}

func (mathSprintLogic) start(seed int64, now time.Time) mathSprintState {
	return mathSprintState{Seed: seed, Deadline: now.Add(mathSprintDuration)}
}

func (mathSprintLogic) deadline(s mathSprintState) time.Time { return s.Deadline }

func (mathSprintLogic) view(s mathSprintState) any {
	return mathSprintView{
		Question:     mathSprintProblemAt(s.Seed, s.Index).Question,
		Index:        s.Index,
		Correct:      s.Correct,
		Wrong:        s.Wrong,
		Deadline:     s.Deadline,
		DurationSecs: int(mathSprintDuration.Seconds()),
	}
}

func (mathSprintLogic) move(s mathSprintState, raw json.RawMessage, now time.Time) (mathSprintState, Outcome, error) {
	// The deadline is checked against the server's clock, so a client that stops calling home
	// simply stops scoring. There is nothing to tamper with on the client side.
	if now.After(s.Deadline) {
		return s, Outcome{}, ErrDeadlinePassed
	}

	move, err := decodeMove[mathSprintMove](raw)
	if err != nil {
		return s, Outcome{}, err
	}
	if move.Answer == nil {
		return s, Outcome{}, fmt.Errorf("%w: an answer is required", ErrInvalidMove)
	}

	correct := *move.Answer == mathSprintProblemAt(s.Seed, s.Index).Answer
	if correct {
		s.Correct++
	} else {
		s.Wrong++
	}
	s.Index++

	return s, Outcome{Correct: correct}, nil
}

func (mathSprintLogic) raw(s mathSprintState) float64 { return float64(s.Correct) }

func mathSprintDefinition() Definition {
	return Definition{
		Slug:    MathSprint,
		Name:    "Math Sprint",
		Summary: "Answer as much arithmetic as you can in thirty seconds.",
		Rules: []string{
			"You have 30 seconds from the moment the session starts.",
			"Each answer immediately returns the next question.",
			"Addition, subtraction and multiplication, all small enough to do in your head.",
			"A wrong answer costs you nothing but the time you spent on it.",
			"Your score is the number of correct answers before the clock runs out.",
		},
		Metric:        "correct answers",
		LowerIsBetter: false,
		Duration:      mathSprintDuration,
		engine:        engineFor[mathSprintState](mathSprintLogic{}),
		points: func(raw float64) float64 {
			return scale(raw, 0, mathSprintTarget)
		},
	}
}
