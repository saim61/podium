package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/saim61/podium/internal/auth"
	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/httpapi"
	"github.com/saim61/podium/internal/leaderboard"
	"github.com/saim61/podium/internal/ratelimit"
	"github.com/saim61/podium/internal/realtime"
	"github.com/saim61/podium/internal/session"
	"github.com/saim61/podium/internal/testsupport"
	"github.com/saim61/podium/internal/user"
)

// instance is one Podium API process, served over a real HTTP listener so WebSockets work.
type instance struct {
	server *httptest.Server
	board  *leaderboard.Board
	hub    *realtime.Hub
	cfg    config.Config
}

// newInstances builds n independent API instances sharing one Postgres and one Redis, which is
// the arrangement the Pub/Sub design exists for.
func newInstances(t *testing.T, n int) []*instance {
	t.Helper()

	t.Setenv("PODIUM_ARGON2_MEMORY_KIB", "8192")
	t.Setenv("PODIUM_ARGON2_ITERATIONS", "1")
	t.Setenv("PODIUM_ARGON2_PARALLELISM", "1")
	t.Setenv("PODIUM_RT_FLUSH_INTERVAL", "25ms")

	pool := testsupport.Postgres(t)
	rdb := testsupport.Redis(t)

	cfg, err := config.Load()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	instances := make([]*instance, 0, n)
	for range n {
		authService, err := auth.NewService(pool, cfg.Auth)
		require.NoError(t, err)

		limiter, err := ratelimit.New(rdb, cfg.Redis.OpTimeout)
		require.NoError(t, err)

		registry := games.NewRegistry()

		board, err := leaderboard.New(rdb, user.NewDirectory(pool),
			leaderboard.WithAnnouncer(realtime.NewPublisher(rdb, cfg.Redis.OpTimeout)))
		require.NoError(t, err)

		hub := realtime.NewHub(board, cfg.Realtime, discard())
		bridge := realtime.NewBridge(rdb, hub, registry, discard())
		tickets := realtime.NewTickets(rdb, cfg.Realtime, cfg.Redis.OpTimeout)

		go func() { _ = hub.Run(ctx) }()
		go func() { _ = bridge.Run(ctx) }()

		sessions := session.NewService(pool, registry,
			session.WithProjector(board),
			session.WithProjectTimeout(cfg.Redis.OpTimeout))

		router := httpapi.NewRouter(httpapi.Deps{
			Config:      cfg,
			Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
			Auth:        authService,
			Sessions:    sessions,
			Leaderboard: board,
			Realtime:    realtime.NewServer(hub, tickets, registry, cfg.Realtime, discard()),
			Tickets:     tickets,
			Limiter:     limiter,
		})

		server := httptest.NewServer(router)
		t.Cleanup(server.Close)

		instances = append(instances, &instance{server: server, board: board, hub: hub, cfg: cfg})
	}

	// The bridge confirms its subscription before serving, but the goroutines above have not
	// necessarily reached that point yet. Waiting avoids a publish that lands before anybody
	// is listening.
	time.Sleep(150 * time.Millisecond)

	return instances
}

func (i *instance) post(t *testing.T, path, token string, body any) *http.Response {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		reader = strings.NewReader(string(encoded))
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, i.server.URL+path, reader)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeBody[T any](t *testing.T, resp *http.Response) T {
	t.Helper()

	var out T
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, &out), string(body))
	return out
}

func (i *instance) registerPlayer(t *testing.T, username string) string {
	t.Helper()

	resp := i.post(t, "/v1/auth/register", "", registration(username))
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	return decodeBody[authBody](t, resp).Tokens.AccessToken
}

func (i *instance) ticket(t *testing.T, token string) string {
	t.Helper()

	resp := i.post(t, "/v1/realtime/ticket", token, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	return decodeBody[struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expires_in"`
	}](t, resp).Ticket
}

// connect opens a WebSocket to an instance and subscribes to channels.
func (i *instance) connect(t *testing.T, ticket string, channels ...string) *websocket.Conn {
	t.Helper()

	url := "ws" + strings.TrimPrefix(i.server.URL, "http") + "/v1/ws?ticket=" + ticket

	conn, _, err := websocket.Dial(t.Context(), url, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	if len(channels) > 0 {
		require.NoError(t, sendOp(t, conn, "subscribe", channels...))
	}
	return conn
}

func sendOp(t *testing.T, conn *websocket.Conn, op string, channels ...string) error {
	t.Helper()

	payload, err := json.Marshal(map[string]any{"op": op, "channels": channels})
	require.NoError(t, err)

	return conn.Write(t.Context(), websocket.MessageText, payload)
}

type frame struct {
	Type       string              `json:"type"`
	Channel    string              `json:"channel"`
	Total      int64               `json:"total"`
	Entries    []leaderboard.Entry `json:"entries"`
	Message    string              `json:"message"`
	Subscribed []string            `json:"subscribed"`
}

// awaitFrame reads until a frame of the wanted type arrives, or the deadline passes.
func awaitFrame(t *testing.T, conn *websocket.Conn, wantType string, timeout time.Duration) frame {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	for {
		_, data, err := conn.Read(ctx)
		require.NoError(t, err, "waiting for a %q frame", wantType)

		var received frame
		require.NoError(t, json.Unmarshal(data, &received))

		if received.Type == wantType {
			return received
		}
	}
}

func TestTicketIsRequiredToOpenASocket(t *testing.T) {
	one := newInstances(t, 1)[0]

	url := "ws" + strings.TrimPrefix(one.server.URL, "http") + "/v1/ws"

	for name, query := range map[string]string{
		"no ticket":      "",
		"unknown ticket": "?ticket=nonsense",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := websocket.Dial(t.Context(), url+query, nil)
			require.Error(t, err, "a handshake without a valid ticket must be refused")
		})
	}
}

func TestTicketEndpointNeedsAuthentication(t *testing.T) {
	one := newInstances(t, 1)[0]

	resp := one.post(t, "/v1/realtime/ticket", "", nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// Single use, so a ticket captured from a log or a referrer header is worthless the moment the
// legitimate client has used it.
func TestATicketWorksExactlyOnce(t *testing.T) {
	one := newInstances(t, 1)[0]
	token := one.registerPlayer(t, "saeem")
	ticket := one.ticket(t, token)

	first := one.connect(t, ticket)
	require.NotNil(t, first)

	url := "ws" + strings.TrimPrefix(one.server.URL, "http") + "/v1/ws?ticket=" + ticket
	_, _, err := websocket.Dial(t.Context(), url, nil)
	require.Error(t, err, "a spent ticket must not open a second socket")
}

func TestSubscribingSendsTheBoardImmediately(t *testing.T) {
	one := newInstances(t, 1)[0]
	token := one.registerPlayer(t, "saeem")

	conn := one.connect(t, one.ticket(t, token), "memory:all-time")

	// A quiet board still has to render, so the opening snapshot cannot wait for a change.
	snapshot := awaitFrame(t, conn, "snapshot", 5*time.Second)
	require.Equal(t, "memory:all-time", snapshot.Channel)
	require.Zero(t, snapshot.Total)
	require.Empty(t, snapshot.Entries)
}

func TestSubscriptionIsConfirmed(t *testing.T) {
	one := newInstances(t, 1)[0]
	token := one.registerPlayer(t, "saeem")

	conn := one.connect(t, one.ticket(t, token), "memory:daily")

	confirmed := awaitFrame(t, conn, "subscribed", 5*time.Second)
	require.Equal(t, []string{"memory:daily"}, confirmed.Subscribed)
}

func TestPlayingAGamePushesASnapshot(t *testing.T) {
	one := newInstances(t, 1)[0]
	token := one.registerPlayer(t, "saeem")

	conn := one.connect(t, one.ticket(t, token), "memory:all-time")
	awaitFrame(t, conn, "snapshot", 5*time.Second)

	playMemoryOver(t, one, token, 4)

	pushed := awaitFrame(t, conn, "snapshot", 5*time.Second)
	require.Equal(t, int64(1), pushed.Total)
	require.Equal(t, "saeem", pushed.Entries[0].Username)
	require.Positive(t, pushed.Entries[0].Points)
}

// playMemoryOver plays memory through one instance's HTTP API.
func playMemoryOver(t *testing.T, i *instance, token string, levels int) {
	t.Helper()

	resp := i.post(t, "/v1/games/memory/sessions", token, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	opened := decodeBody[sessionBody](t, resp)

	current := opened
	for range levels {
		var view struct {
			Sequence []int `json:"sequence"`
		}
		require.NoError(t, json.Unmarshal(current.State, &view))

		moved := i.post(t, "/v1/sessions/"+opened.ID.String()+"/moves", token,
			map[string][]int{"answer": view.Sequence})
		require.Equal(t, http.StatusOK, moved.StatusCode)
		current = decodeBody[sessionBody](t, moved)
	}

	finished := i.post(t, "/v1/sessions/"+opened.ID.String()+"/finish", token, nil)
	require.Equal(t, http.StatusOK, finished.StatusCode)
}

// The test this whole design exists for. A hub that only broadcast what its own process scored
// would pass every single-instance test above and silently fail here.
func TestAScoreOnOneInstanceReachesASocketOnAnother(t *testing.T) {
	instances := newInstances(t, 2)
	a, b := instances[0], instances[1]

	// The listener is on B.
	watcher := b.registerPlayer(t, "watcher")
	conn := b.connect(t, b.ticket(t, watcher), "memory:all-time")
	awaitFrame(t, conn, "snapshot", 5*time.Second)

	// The player is on A, and never touches B.
	player := a.registerPlayer(t, "player")
	playMemoryOver(t, a, player, 5)

	pushed := awaitFrame(t, conn, "snapshot", 10*time.Second)

	require.Equal(t, int64(1), pushed.Total)
	require.Equal(t, "player", pushed.Entries[0].Username,
		"instance B must learn about a score submitted to instance A")
}

func TestBothInstancesSeeTheSameChange(t *testing.T) {
	instances := newInstances(t, 2)
	a, b := instances[0], instances[1]

	onA := a.connect(t, a.ticket(t, a.registerPlayer(t, "watchera")), "global:all-time")
	onB := b.connect(t, b.ticket(t, b.registerPlayer(t, "watcherb")), "global:all-time")

	awaitFrame(t, onA, "snapshot", 5*time.Second)
	awaitFrame(t, onB, "snapshot", 5*time.Second)

	player := a.registerPlayer(t, "player")
	playMemoryOver(t, a, player, 4)

	fromA := awaitFrame(t, onA, "snapshot", 10*time.Second)
	fromB := awaitFrame(t, onB, "snapshot", 10*time.Second)

	require.Equal(t, fromA.Entries, fromB.Entries,
		"every instance serves the same board because they all read the same Redis")
}

func TestUnsubscribeStopsUpdates(t *testing.T) {
	one := newInstances(t, 1)[0]
	token := one.registerPlayer(t, "saeem")

	conn := one.connect(t, one.ticket(t, token), "memory:all-time")
	awaitFrame(t, conn, "subscribed", 5*time.Second)

	require.NoError(t, sendOp(t, conn, "unsubscribe", "memory:all-time"))
	confirmed := awaitFrame(t, conn, "subscribed", 5*time.Second)
	require.Empty(t, confirmed.Subscribed)

	require.Eventually(t, func() bool {
		return one.hub.Subscribers(realtime.Channel{
			Scope:  leaderboard.Game(games.Memory),
			Period: leaderboard.AllTime,
		}) == 0
	}, 2*time.Second, 20*time.Millisecond)
}

func TestUnknownChannelIsRejectedWithoutClosingTheSocket(t *testing.T) {
	one := newInstances(t, 1)[0]
	token := one.registerPlayer(t, "saeem")

	conn := one.connect(t, one.ticket(t, token))
	require.NoError(t, sendOp(t, conn, "subscribe", "pinball:daily"))

	rejected := awaitFrame(t, conn, "error", 5*time.Second)
	require.Contains(t, rejected.Message, "pinball")

	// The connection has to survive a bad request, or one typo costs a reconnect.
	require.NoError(t, sendOp(t, conn, "subscribe", "memory:daily"))
	awaitFrame(t, conn, "snapshot", 5*time.Second)
}

func TestUnknownOpIsRejected(t *testing.T) {
	one := newInstances(t, 1)[0]
	token := one.registerPlayer(t, "saeem")

	conn := one.connect(t, one.ticket(t, token))

	payload, err := json.Marshal(map[string]any{"op": "drop-tables"})
	require.NoError(t, err)
	require.NoError(t, conn.Write(t.Context(), websocket.MessageText, payload))

	rejected := awaitFrame(t, conn, "error", 5*time.Second)
	require.Contains(t, rejected.Message, "subscribe")
}

func TestMalformedMessageIsRejected(t *testing.T) {
	one := newInstances(t, 1)[0]
	token := one.registerPlayer(t, "saeem")

	conn := one.connect(t, one.ticket(t, token))
	require.NoError(t, conn.Write(t.Context(), websocket.MessageText, []byte("not json")))

	rejected := awaitFrame(t, conn, "error", 5*time.Second)
	require.Contains(t, rejected.Message, "JSON")
}

func TestChannelCountPerConnectionIsCapped(t *testing.T) {
	one := newInstances(t, 1)[0]
	token := one.registerPlayer(t, "saeem")

	conn := one.connect(t, one.ticket(t, token))

	// The cap is 16 by default; ask for more boards than exist to be sure of crossing it.
	var channels []string
	for _, slug := range games.NewRegistry().Slugs() {
		for _, period := range leaderboard.Periods {
			channels = append(channels, string(slug)+":"+string(period))
		}
	}
	require.Greater(t, len(channels), one.cfg.Realtime.MaxChannels)
	require.NoError(t, sendOp(t, conn, "subscribe", channels...))

	rejected := awaitFrame(t, conn, "error", 5*time.Second)
	require.Contains(t, rejected.Message, "too many channels")
}

func TestPingIsAnswered(t *testing.T) {
	one := newInstances(t, 1)[0]
	token := one.registerPlayer(t, "saeem")

	conn := one.connect(t, one.ticket(t, token))

	payload, err := json.Marshal(map[string]any{"op": "ping"})
	require.NoError(t, err)
	require.NoError(t, conn.Write(t.Context(), websocket.MessageText, payload))

	awaitFrame(t, conn, "pong", 5*time.Second)
}

// A score that beats nothing moves no board, so waking every subscriber to re-render an
// unchanged leaderboard would make the busiest games the noisiest for no reason.
// frameReader reads a connection in the background.
//
// Asserting that nothing arrives cannot be done with a per-read deadline: coder/websocket treats
// a cancelled Read as fatal and closes the connection, because a partly consumed frame leaves
// the stream unrecoverable. So the read stays pending in its own goroutine and the test waits on
// a timer instead.
type frameReader struct {
	frames chan frame
	errs   chan error
}

func readFrames(t *testing.T, conn *websocket.Conn) *frameReader {
	t.Helper()

	r := &frameReader{frames: make(chan frame, 32), errs: make(chan error, 1)}

	go func() {
		for {
			_, data, err := conn.Read(t.Context())
			if err != nil {
				r.errs <- err
				return
			}

			var received frame
			if json.Unmarshal(data, &received) == nil {
				r.frames <- received
			}
		}
	}()
	return r
}

// drainUntilQuiet discards frames until none arrive for the given span.
func (r *frameReader) drainUntilQuiet(quiet time.Duration) {
	for {
		select {
		case <-r.frames:
		case <-time.After(quiet):
			return
		}
	}
}

// expectNo fails if a frame of the given type arrives within the span.
func (r *frameReader) expectNo(t *testing.T, unwanted string, within time.Duration) {
	t.Helper()

	deadline := time.After(within)
	for {
		select {
		case received := <-r.frames:
			require.NotEqual(t, unwanted, received.Type,
				"an unimproved score should not push a new board")
		case err := <-r.errs:
			t.Fatalf("connection failed while waiting: %v", err)
		case <-deadline:
			return
		}
	}
}

func TestAScoreThatBeatsNothingSendsNoUpdate(t *testing.T) {
	one := newInstances(t, 1)[0]
	token := one.registerPlayer(t, "saeem")

	playMemoryOver(t, one, token, 6)

	conn := one.connect(t, one.ticket(t, token), "memory:all-time")

	// Let anything the earlier play announced arrive first. A notification travels through
	// Redis asynchronously, so one published before this socket existed can still land just
	// after it - legitimately, and nothing to do with what is being tested here.
	reader := readFrames(t, conn)
	reader.drainUntilQuiet(400 * time.Millisecond)

	// A worse attempt: the personal best is unchanged.
	playMemoryOver(t, one, token, 1)

	reader.expectNo(t, "snapshot", 1500*time.Millisecond)
}
