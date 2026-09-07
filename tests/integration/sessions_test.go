package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/session"
)

type sessionBody struct {
	ID         uuid.UUID       `json:"id"`
	Game       string          `json:"game"`
	Status     string          `json:"status"`
	Moves      int             `json:"moves"`
	DeadlineAt *time.Time      `json:"deadline_at"`
	State      json.RawMessage `json:"state"`
	Score      *struct {
		Game   string  `json:"game"`
		Metric string  `json:"metric"`
		Raw    float64 `json:"raw"`
		Points int     `json:"points"`
	} `json:"score"`
}

func (h *authHarness) startSession(t *testing.T, token string, slug games.Slug) sessionBody {
	t.Helper()

	rec := h.do(t, http.MethodPost, "/v1/games/"+string(slug)+"/sessions", nil, token)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	return decodeInto[sessionBody](t, rec)
}

func (h *authHarness) move(t *testing.T, token string, id uuid.UUID, move any) sessionBody {
	t.Helper()

	rec := h.do(t, http.MethodPost, "/v1/sessions/"+id.String()+"/moves", move, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return decodeInto[sessionBody](t, rec)
}

func (h *authHarness) finish(t *testing.T, token string, id uuid.UUID) sessionBody {
	t.Helper()

	rec := h.do(t, http.MethodPost, "/v1/sessions/"+id.String()+"/finish", nil, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return decodeInto[sessionBody](t, rec)
}

// secretsFor reads a session's private state straight from the database. Tests need the answers
// to play the games; clients cannot get at them, which is the point.
func (h *authHarness) privateState(t *testing.T, id uuid.UUID) map[string]any {
	t.Helper()

	var raw []byte
	require.NoError(t, h.pool.QueryRow(t.Context(),
		"SELECT state FROM game_sessions WHERE id = $1", id).Scan(&raw))

	var state map[string]any
	require.NoError(t, json.Unmarshal(raw, &state))
	return state
}

func TestGamesAreListedWithoutAuthentication(t *testing.T) {
	h := newAuthHarness(t)

	rec := h.do(t, http.MethodGet, "/v1/games", nil, "")
	require.Equal(t, http.StatusOK, rec.Code)

	body := decodeInto[struct {
		Games []struct {
			Slug      string   `json:"slug"`
			Name      string   `json:"name"`
			Rules     []string `json:"rules"`
			MaxPoints int      `json:"max_points"`
		} `json:"games"`
	}](t, rec)

	require.Len(t, body.Games, 5)
	for _, game := range body.Games {
		require.NotEmpty(t, game.Name, game.Slug)
		require.NotEmpty(t, game.Rules, game.Slug)
		require.Equal(t, games.MaxPoints, game.MaxPoints)
	}
}

func TestUnknownGameIsNotFound(t *testing.T) {
	h := newAuthHarness(t)

	rec := h.do(t, http.MethodGet, "/v1/games/pinball", nil, "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestStartingASessionRequiresAuthentication(t *testing.T) {
	h := newAuthHarness(t)

	rec := h.do(t, http.MethodPost, "/v1/games/memory/sessions", nil, "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// There is no endpoint that accepts a score. This asserts the absence, because the whole
// anti-forgery argument depends on it.
func TestThereIsNoWayToSubmitAScore(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")

	for _, path := range []string{"/v1/scores", "/v1/leaderboards/memory/scores", "/v1/submit"} {
		rec := h.do(t, http.MethodPost, path,
			map[string]any{"game": "memory", "score": 999999}, player.Tokens.AccessToken)

		require.Equal(t, http.StatusNotFound, rec.Code, path)
	}
}

func TestSessionResponseNeverCarriesPrivateState(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")

	opened := h.startSession(t, player.Tokens.AccessToken, games.NumberGuess)

	require.NotContains(t, string(opened.State), "secrets")

	// The answers do exist - on the server.
	require.Contains(t, h.privateState(t, opened.ID), "secrets")
}

func TestNumberGuessPlayedThroughTheAPI(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	token := player.Tokens.AccessToken

	opened := h.startSession(t, token, games.NumberGuess)
	require.Equal(t, session.StatusActive, opened.Status)
	require.Nil(t, opened.Score)

	secrets := h.privateState(t, opened.ID)["secrets"].([]any)
	require.Len(t, secrets, 3)

	var last sessionBody
	for _, secret := range secrets {
		last = h.move(t, token, opened.ID, map[string]int{"guess": int(secret.(float64))})
	}

	require.Equal(t, session.StatusFinished, last.Status,
		"the last correct guess should end the session on its own")
	require.NotNil(t, last.Score)
	require.Equal(t, float64(3), last.Score.Raw)
	require.Equal(t, games.MaxPoints, last.Score.Points)
	require.Equal(t, "guesses", last.Score.Metric)
}

func TestMemoryPlayedThroughTheAPI(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	token := player.Tokens.AccessToken

	opened := h.startSession(t, token, games.Memory)

	current := opened
	for level := 1; level <= 5; level++ {
		var view struct {
			Sequence  []int `json:"sequence"`
			Completed int   `json:"completed"`
		}
		require.NoError(t, json.Unmarshal(current.State, &view))
		require.Len(t, view.Sequence, level)

		current = h.move(t, token, opened.ID, map[string][]int{"answer": view.Sequence})
	}

	finished := h.finish(t, token, opened.ID)
	require.Equal(t, session.StatusFinished, finished.Status)
	require.NotNil(t, finished.Score)
	require.Equal(t, float64(5), finished.Score.Raw)
	require.Positive(t, finished.Score.Points)
}

func TestMathSprintPlayedThroughTheAPI(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	token := player.Tokens.AccessToken

	opened := h.startSession(t, token, games.MathSprint)
	require.NotNil(t, opened.DeadlineAt, "a timed game must publish its deadline")

	current := opened
	for range 4 {
		var view struct {
			Question string `json:"question"`
		}
		require.NoError(t, json.Unmarshal(current.State, &view))

		current = h.move(t, token, opened.ID, map[string]int{"answer": solve(t, view.Question)})
	}

	finished := h.finish(t, token, opened.ID)
	require.Equal(t, float64(4), finished.Score.Raw)
	require.Equal(t, "correct answers", finished.Score.Metric)
}

// solve evaluates the questions math-sprint actually asks, so the test plays the game rather
// than reaching into the engine for the answer.
func solve(t *testing.T, question string) int {
	t.Helper()

	var a, b int
	var op string
	_, err := fmt.Sscan(question, &a, &op, &b)
	require.NoError(t, err, question)

	switch op {
	case "+":
		return a + b
	case "-":
		return a - b
	case "x":
		return a * b
	default:
		t.Fatalf("unexpected operator %q in %q", op, question)
		return 0
	}
}

func TestWordScramblePlayedThroughTheAPI(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	token := player.Tokens.AccessToken

	opened := h.startSession(t, token, games.WordScramble)

	// Skipping is a legal move and must advance the puzzle without scoring.
	after := h.move(t, token, opened.ID, map[string]bool{"skip": true})

	var view struct {
		Index   int `json:"index"`
		Skipped int `json:"skipped"`
		Solved  int `json:"solved"`
	}
	require.NoError(t, json.Unmarshal(after.State, &view))
	require.Equal(t, 1, view.Index)
	require.Equal(t, 1, view.Skipped)

	finished := h.finish(t, token, opened.ID)
	require.Equal(t, float64(0), finished.Score.Raw)
	require.Equal(t, 0, finished.Score.Points)
}

func TestReactionHoldsTheSignalBeforeAcceptingATap(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	token := player.Tokens.AccessToken

	opened := h.startSession(t, token, games.Reaction)

	armed := h.move(t, token, opened.ID, map[string]string{"action": "arm"})

	var view struct {
		Armed bool   `json:"armed"`
		Next  string `json:"next"`
	}
	require.NoError(t, json.Unmarshal(armed.State, &view))
	require.True(t, view.Armed)
	require.Equal(t, "tap", view.Next)
	require.NotContains(t, string(armed.State), "go_at")

	// The engine asked for a hold of between 1.2 and 3 seconds.
	holds := h.holds.all()
	require.Len(t, holds, 1)
	require.WithinRange(t, holds[0], time.Now().Add(time.Second), time.Now().Add(4*time.Second))

	tapped := h.move(t, token, opened.ID, map[string]string{"action": "tap"})

	var results struct {
		Results []int64 `json:"results"`
		Fouls   int     `json:"fouls"`
	}
	require.NoError(t, json.Unmarshal(tapped.State, &results))
	require.Len(t, results.Results, 1)

	// The stubbed sleeper returned instantly, so the tap arrives before the signal was due and
	// is correctly charged as a foul.
	require.Equal(t, 1, results.Fouls)
}

// The hold is a security property, not a nicety: without it a client learns nothing about when
// the signal is due, but also never has to wait for it. This proves the wait really happens.
func TestReactionHoldActuallyDelaysTheResponse(t *testing.T) {
	h := newAuthHarness(t, honourHolds)
	player := h.register(t, "saeem")
	token := player.Tokens.AccessToken

	opened := h.startSession(t, token, games.Reaction)

	before := time.Now()
	h.move(t, token, opened.ID, map[string]string{"action": "arm"})
	elapsed := time.Since(before)

	require.GreaterOrEqual(t, elapsed, 1200*time.Millisecond,
		"arming must not return before the signal is due")
	require.Less(t, elapsed, 5*time.Second)
}

func TestSessionsBelongToOneUser(t *testing.T) {
	h := newAuthHarness(t)
	owner := h.register(t, "owner")
	intruder := h.register(t, "intruder")

	opened := h.startSession(t, owner.Tokens.AccessToken, games.Memory)

	for name, request := range map[string]struct {
		method, path string
		body         any
	}{
		"read":   {http.MethodGet, "/v1/sessions/" + opened.ID.String(), nil},
		"move":   {http.MethodPost, "/v1/sessions/" + opened.ID.String() + "/moves", map[string][]int{"answer": {1}}},
		"finish": {http.MethodPost, "/v1/sessions/" + opened.ID.String() + "/finish", nil},
	} {
		t.Run(name, func(t *testing.T) {
			rec := h.do(t, request.method, request.path, request.body, intruder.Tokens.AccessToken)

			require.Equal(t, http.StatusNotFound, rec.Code,
				"another user's session must look absent, not forbidden")
		})
	}
}

func TestUnknownSessionIsNotFound(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")

	rec := h.do(t, http.MethodGet, "/v1/sessions/"+uuid.NewString(), nil, player.Tokens.AccessToken)
	require.Equal(t, http.StatusNotFound, rec.Code)

	malformed := h.do(t, http.MethodGet, "/v1/sessions/not-a-uuid", nil, player.Tokens.AccessToken)
	require.Equal(t, http.StatusNotFound, malformed.Code)
}

func TestFinishIsIdempotent(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	token := player.Tokens.AccessToken

	opened := h.startSession(t, token, games.Memory)

	first := h.finish(t, token, opened.ID)
	second := h.finish(t, token, opened.ID)

	require.Equal(t, first.Score.Points, second.Score.Points)
	require.Equal(t, first.Score.Raw, second.Score.Raw)
	require.Equal(t, 1, h.scoreEventCount(t, opened.ID),
		"finishing twice must not write two scores")
}

func (h *authHarness) scoreEventCount(t *testing.T, id uuid.UUID) int {
	t.Helper()

	var count int
	require.NoError(t, h.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM score_events WHERE session_id = $1", id).Scan(&count))
	return count
}

// Two clients finishing at once must still produce exactly one score. The unique index on
// session_id is what makes that true regardless of how the reads interleave.
func TestConcurrentFinishesRecordOneScore(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	token := player.Tokens.AccessToken

	opened := h.startSession(t, token, games.Memory)

	const attempts = 8
	codes := make([]int, attempts)

	var wg sync.WaitGroup
	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = h.do(t, http.MethodPost,
				"/v1/sessions/"+opened.ID.String()+"/finish", nil, token).Code
		}(i)
	}
	wg.Wait()

	require.Equal(t, 1, h.scoreEventCount(t, opened.ID))
	for _, code := range codes {
		require.Contains(t, []int{http.StatusOK, http.StatusConflict}, code)
	}
}

func TestMovesAreRefusedAfterFinishing(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	token := player.Tokens.AccessToken

	opened := h.startSession(t, token, games.Memory)
	h.finish(t, token, opened.ID)

	rec := h.do(t, http.MethodPost, "/v1/sessions/"+opened.ID.String()+"/moves",
		map[string][]int{"answer": {1}}, token)

	require.Equal(t, http.StatusConflict, rec.Code)
}

func TestStartingASecondSessionAbandonsTheFirst(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	token := player.Tokens.AccessToken

	first := h.startSession(t, token, games.Memory)
	second := h.startSession(t, token, games.Memory)

	require.NotEqual(t, first.ID, second.ID)
	require.Equal(t, session.StatusAbandoned, h.sessionStatus(t, first.ID))
	require.Equal(t, session.StatusActive, h.sessionStatus(t, second.ID))

	// An abandoned session cannot be played or scored.
	rec := h.do(t, http.MethodPost, "/v1/sessions/"+first.ID.String()+"/moves",
		map[string][]int{"answer": {1}}, token)
	require.Equal(t, http.StatusConflict, rec.Code)

	require.Zero(t, h.scoreEventCount(t, first.ID))
}

// A session for one game must not disturb a session for another.
func TestSessionsForDifferentGamesCoexist(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	token := player.Tokens.AccessToken

	memory := h.startSession(t, token, games.Memory)
	sprint := h.startSession(t, token, games.MathSprint)

	require.Equal(t, session.StatusActive, h.sessionStatus(t, memory.ID))
	require.Equal(t, session.StatusActive, h.sessionStatus(t, sprint.ID))
}

func (h *authHarness) sessionStatus(t *testing.T, id uuid.UUID) string {
	t.Helper()

	var status string
	require.NoError(t, h.pool.QueryRow(t.Context(),
		"SELECT status FROM game_sessions WHERE id = $1", id).Scan(&status))
	return status
}

func TestInvalidMovesAreRejected(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	token := player.Tokens.AccessToken

	opened := h.startSession(t, token, games.NumberGuess)

	for name, move := range map[string]any{
		"out of range":  map[string]int{"guess": 500},
		"unknown field": map[string]int{"guesss": 5},
		"wrong shape":   map[string]string{"guess": "fifty"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := h.do(t, http.MethodPost,
				"/v1/sessions/"+opened.ID.String()+"/moves", move, token)

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}
}

func TestGetSessionReturnsCurrentView(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	token := player.Tokens.AccessToken

	opened := h.startSession(t, token, games.NumberGuess)
	h.move(t, token, opened.ID, map[string]int{"guess": 50})

	rec := h.do(t, http.MethodGet, "/v1/sessions/"+opened.ID.String(), nil, token)
	require.Equal(t, http.StatusOK, rec.Code)

	fetched := decodeInto[sessionBody](t, rec)
	require.Equal(t, 1, fetched.Moves)
	require.NotContains(t, string(fetched.State), "secrets")

	var view struct {
		GuessesUsed int    `json:"guesses_used"`
		Hint        string `json:"hint"`
	}
	require.NoError(t, json.Unmarshal(fetched.State, &view))
	require.Equal(t, 1, view.GuessesUsed)
	require.NotEmpty(t, view.Hint)
}

// Every game must record a score row with points inside the shared scale, since phase 4 projects
// exactly this column into Redis.
func TestEveryGameRecordsAScoreEvent(t *testing.T) {
	h := newAuthHarness(t)
	player := h.register(t, "saeem")
	token := player.Tokens.AccessToken

	for _, slug := range games.NewRegistry().Slugs() {
		t.Run(string(slug), func(t *testing.T) {
			opened := h.startSession(t, token, slug)
			finished := h.finish(t, token, opened.ID)

			require.NotNil(t, finished.Score)
			require.Equal(t, string(slug), finished.Score.Game)
			require.GreaterOrEqual(t, finished.Score.Points, 0)
			require.LessOrEqual(t, finished.Score.Points, games.MaxPoints)
			require.Equal(t, 1, h.scoreEventCount(t, opened.ID))
		})
	}
}
