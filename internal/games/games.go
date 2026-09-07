// Package games holds Podium's five games. Each is a pure state machine: it takes state and a
// move and returns new state, with no database, clock or network of its own.
//
// The important split is between State and View. State is private to the server and holds the
// answers - the number to guess, the sequence, the target word. View is the projection a client
// is allowed to see. A client cannot forge a score because it never sends one, and cannot skip
// ahead because the answers were never in its possession.
package games

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Slug identifies a game in URLs and on leaderboards.
type Slug string

// The five games.
const (
	Reaction     Slug = "reaction"
	MathSprint   Slug = "math-sprint"
	Memory       Slug = "memory"
	WordScramble Slug = "word-scramble"
	NumberGuess  Slug = "number-guess"
)

// Failures an engine can report.
var (
	ErrInvalidMove    = errors.New("invalid move")
	ErrNoSuchGame     = errors.New("no such game")
	ErrDeadlinePassed = errors.New("the deadline has passed")
	ErrAlreadyDone    = errors.New("this session has already ended")
)

// MaxPoints is the top of the normalised scale every game maps onto.
const MaxPoints = 10_000

// State is a game's private state, persisted as JSON. It contains answers and never leaves the
// server.
type State = json.RawMessage

// View is the part of a game's state a client may see.
type View = json.RawMessage

// Outcome describes what happened after a move.
type Outcome struct {
	// Correct reports whether the move itself was right, for games where that is meaningful.
	Correct bool

	// Done means the game has ended on its own, with no deadline involved.
	Done bool

	// HoldUntil asks the transport to delay its response until this instant. Reaction time
	// needs the response itself to be the "go" signal, and an engine cannot sleep - so it
	// declares the delay and the caller performs it. This keeps the engine pure and lets tests
	// assert on the instant rather than waiting for it.
	HoldUntil time.Time
}

// Engine is a game's state machine.
type Engine interface {
	Start(seed int64, now time.Time) (State, View, time.Time, error)
	Move(state State, move json.RawMessage, now time.Time) (State, View, Outcome, error)
	View(state State) (View, error)
	Raw(state State) (float64, error)
}

// Definition is a game's metadata and its mapping from raw metric to points.
type Definition struct {
	Slug          Slug
	Name          string
	Summary       string
	Rules         []string
	Metric        string
	LowerIsBetter bool
	Duration      time.Duration
	engine        Engine
	points        func(raw float64) float64
}

// Engine returns the game's state machine.
func (d Definition) Engine() Engine { return d.engine }

// Points converts a raw metric to the shared 0-10,000 scale.
//
// Normalisation exists so the cross-game leaderboard can add scores together at all: 210
// milliseconds and 14 solved anagrams have no common unit until one is invented. It also means
// Redis only ever stores a higher-is-better number, so a sorted set needs no per-game special
// casing for games where lower is better.
func (d Definition) Points(raw float64) int {
	points := d.points(raw)

	switch {
	case points < 0:
		return 0
	case points > MaxPoints:
		return MaxPoints
	default:
		return int(points)
	}
}

// scale maps raw linearly onto 0-MaxPoints, where worst earns nothing and best earns everything.
// It works in either direction: pass best < worst for a lower-is-better metric.
func scale(raw, worst, best float64) float64 {
	if worst == best {
		return 0
	}
	return MaxPoints * (raw - worst) / (best - worst)
}

// Registry is the set of games Podium offers.
type Registry struct {
	order       []Slug
	definitions map[Slug]Definition
}

// NewRegistry builds the registry of all five games.
func NewRegistry() *Registry {
	definitions := []Definition{
		reactionDefinition(),
		mathSprintDefinition(),
		memoryDefinition(),
		wordScrambleDefinition(),
		numberGuessDefinition(),
	}

	r := &Registry{
		order:       make([]Slug, 0, len(definitions)),
		definitions: make(map[Slug]Definition, len(definitions)),
	}
	for _, d := range definitions {
		r.order = append(r.order, d.Slug)
		r.definitions[d.Slug] = d
	}
	return r
}

// Get returns a game by slug.
func (r *Registry) Get(slug Slug) (Definition, error) {
	d, ok := r.definitions[slug]
	if !ok {
		return Definition{}, fmt.Errorf("%w: %q", ErrNoSuchGame, slug)
	}
	return d, nil
}

// All returns every game in display order.
func (r *Registry) All() []Definition {
	all := make([]Definition, 0, len(r.order))
	for _, slug := range r.order {
		all = append(all, r.definitions[slug])
	}
	return all
}

// Slugs returns every game's slug in display order.
func (r *Registry) Slugs() []Slug {
	return append([]Slug(nil), r.order...)
}

// logic is a game implemented over its own typed state. The adapter below handles the JSON so no
// engine has to.
type logic[S any] interface {
	start(seed int64, now time.Time) S
	deadline(s S) time.Time
	view(s S) any
	move(s S, move json.RawMessage, now time.Time) (S, Outcome, error)
	raw(s S) float64
}

type adapter[S any] struct {
	l logic[S]
}

// engineFor wraps typed game logic as an Engine.
func engineFor[S any](l logic[S]) Engine { return adapter[S]{l: l} }

func (a adapter[S]) Start(seed int64, now time.Time) (State, View, time.Time, error) {
	s := a.l.start(seed, now)

	state, view, err := a.encode(s)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	return state, view, a.l.deadline(s), nil
}

func (a adapter[S]) Move(state State, move json.RawMessage, now time.Time) (State, View, Outcome, error) {
	current, err := a.decode(state)
	if err != nil {
		return nil, nil, Outcome{}, err
	}

	next, outcome, err := a.l.move(current, move, now)
	if err != nil {
		return nil, nil, Outcome{}, err
	}

	nextState, view, err := a.encode(next)
	if err != nil {
		return nil, nil, Outcome{}, err
	}
	return nextState, view, outcome, nil
}

func (a adapter[S]) View(state State) (View, error) {
	current, err := a.decode(state)
	if err != nil {
		return nil, err
	}

	view, err := json.Marshal(a.l.view(current))
	if err != nil {
		return nil, fmt.Errorf("encode game view: %w", err)
	}
	return view, nil
}

func (a adapter[S]) Raw(state State) (float64, error) {
	current, err := a.decode(state)
	if err != nil {
		return 0, err
	}
	return a.l.raw(current), nil
}

func (a adapter[S]) encode(s S) (State, View, error) {
	state, err := json.Marshal(s)
	if err != nil {
		return nil, nil, fmt.Errorf("encode game state: %w", err)
	}

	view, err := json.Marshal(a.l.view(s))
	if err != nil {
		return nil, nil, fmt.Errorf("encode game view: %w", err)
	}
	return state, view, nil
}

func (a adapter[S]) decode(state State) (S, error) {
	var s S
	if err := json.Unmarshal(state, &s); err != nil {
		return s, fmt.Errorf("decode game state: %w", err)
	}
	return s, nil
}

func decodeMove[M any](raw json.RawMessage) (M, error) {
	var move M
	if len(raw) == 0 {
		return move, fmt.Errorf("%w: a move is required", ErrInvalidMove)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&move); err != nil {
		return move, fmt.Errorf("%w: %w", ErrInvalidMove, err)
	}
	return move, nil
}
