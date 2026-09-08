package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/leaderboard"
)

func discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func testConfig() config.Realtime {
	return config.Realtime{
		FlushInterval: 5 * time.Millisecond,
		TopN:          10,
		SendBuffer:    4,
		TicketTTL:     30 * time.Second,
		MaxChannels:   3,
		WriteTimeout:  time.Second,
		PingInterval:  time.Minute,
	}
}

// fakeBoards stands in for Redis. The hub's job is coalescing and fan-out, and those are worth
// testing without a container in the way.
type fakeBoards struct {
	mu     sync.Mutex
	reads  int32
	page   leaderboard.Page
	err    error
	onRead func()
}

func (f *fakeBoards) Page(_ context.Context, scope leaderboard.Scope, period leaderboard.Period, _, _ int) (leaderboard.Page, error) {
	atomic.AddInt32(&f.reads, 1)

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.onRead != nil {
		f.onRead()
	}
	if f.err != nil {
		return leaderboard.Page{}, f.err
	}

	page := f.page
	page.Scope = scope.Name()
	page.Period = period
	return page, nil
}

func (f *fakeBoards) readCount() int { return int(atomic.LoadInt32(&f.reads)) }

func memoryDaily() Channel {
	return Channel{Scope: leaderboard.Game(games.Memory), Period: leaderboard.Daily}
}

func newTestSubscriber(buffer int) (*subscriber, *atomic.Bool) {
	var dropped atomic.Bool
	return &subscriber{
		send: make(chan []byte, buffer),
		drop: func() { dropped.Store(true) },
	}, &dropped
}

func TestChannelNames(t *testing.T) {
	require.Equal(t, "memory:daily", memoryDaily().String())
	require.Equal(t, "global:all-time",
		Channel{Scope: leaderboard.Global(), Period: leaderboard.AllTime}.String())
}

func TestParseChannel(t *testing.T) {
	registry := games.NewRegistry()

	parsed, err := ParseChannel("memory:daily", registry)
	require.NoError(t, err)
	require.Equal(t, memoryDaily(), parsed)

	global, err := ParseChannel("global:all-time", registry)
	require.NoError(t, err)
	require.True(t, global.Scope.IsGlobal())
}

// A client must not be able to make the hub read arbitrary Redis keys by naming them.
func TestParseChannelRejectsAnythingThatIsNotABoard(t *testing.T) {
	registry := games.NewRegistry()

	for _, raw := range []string{
		"",
		"memory",
		"memory:hourly",
		"pinball:daily",
		"lb:g:memory:all",
		":",
		"global:",
	} {
		_, err := ParseChannel(raw, registry)
		require.Error(t, err, raw)
	}
}

func TestSubscribeAndUnsubscribe(t *testing.T) {
	hub := NewHub(&fakeBoards{}, testConfig(), discard())
	sub, _ := newTestSubscriber(4)

	require.Zero(t, hub.Subscribers(memoryDaily()))

	hub.Subscribe(sub, memoryDaily())
	require.Equal(t, 1, hub.Subscribers(memoryDaily()))

	hub.Unsubscribe(sub, memoryDaily())
	require.Zero(t, hub.Subscribers(memoryDaily()))
}

func TestRemoveDropsEverySubscription(t *testing.T) {
	hub := NewHub(&fakeBoards{}, testConfig(), discard())
	sub, _ := newTestSubscriber(4)

	global := Channel{Scope: leaderboard.Global(), Period: leaderboard.AllTime}
	hub.Subscribe(sub, memoryDaily())
	hub.Subscribe(sub, global)

	hub.Remove(sub)

	require.Zero(t, hub.Subscribers(memoryDaily()))
	require.Zero(t, hub.Subscribers(global))
}

func TestFlushSendsASnapshotToSubscribers(t *testing.T) {
	boards := &fakeBoards{page: leaderboard.Page{
		Total:   2,
		Entries: []leaderboard.Entry{{Rank: 1, Username: "alice", Points: 900}},
	}}
	hub := NewHub(boards, testConfig(), discard())

	sub, _ := newTestSubscriber(4)
	hub.Subscribe(sub, memoryDaily())
	hub.MarkDirty(memoryDaily())

	hub.Flush(context.Background())

	var snapshot Snapshot
	require.NoError(t, json.Unmarshal(<-sub.send, &snapshot))
	require.Equal(t, "snapshot", snapshot.Type)
	require.Equal(t, "memory:daily", snapshot.Channel)
	require.Equal(t, int64(2), snapshot.Total)
	require.Equal(t, "alice", snapshot.Entries[0].Username)
}

// Coalescing is the point of the flush ticker: a hundred scores inside one window must cost one
// read and one frame, not a hundred of each.
func TestManyChangesInOneWindowSendOneFrame(t *testing.T) {
	boards := &fakeBoards{}
	hub := NewHub(boards, testConfig(), discard())

	sub, _ := newTestSubscriber(4)
	hub.Subscribe(sub, memoryDaily())

	for range 100 {
		hub.MarkDirty(memoryDaily())
	}

	hub.Flush(context.Background())

	require.Equal(t, 1, boards.readCount(), "one read per channel per flush")
	require.Len(t, sub.send, 1, "one frame per channel per flush")
}

// Reading Redis for a board nobody is watching is pure waste.
func TestFlushSkipsChannelsWithNoSubscribers(t *testing.T) {
	boards := &fakeBoards{}
	hub := NewHub(boards, testConfig(), discard())

	hub.MarkDirty(memoryDaily())
	hub.Flush(context.Background())

	require.Zero(t, boards.readCount())
}

func TestFlushClearsTheDirtySet(t *testing.T) {
	boards := &fakeBoards{}
	hub := NewHub(boards, testConfig(), discard())

	sub, _ := newTestSubscriber(4)
	hub.Subscribe(sub, memoryDaily())
	hub.MarkDirty(memoryDaily())

	hub.Flush(context.Background())
	hub.Flush(context.Background())

	require.Equal(t, 1, boards.readCount(), "an unchanged board should not be re-read")
}

func TestFlushDeliversToEverySubscriberOfAChannel(t *testing.T) {
	hub := NewHub(&fakeBoards{}, testConfig(), discard())

	first, _ := newTestSubscriber(4)
	second, _ := newTestSubscriber(4)
	hub.Subscribe(first, memoryDaily())
	hub.Subscribe(second, memoryDaily())
	hub.MarkDirty(memoryDaily())

	hub.Flush(context.Background())

	require.Len(t, first.send, 1)
	require.Len(t, second.send, 1)
}

// The classic hub bug: one client that stops reading freezes the fan-out for everybody. A full
// buffer must evict that client, not block.
func TestASlowSubscriberIsDroppedRatherThanWaitedOn(t *testing.T) {
	hub := NewHub(&fakeBoards{}, testConfig(), discard())

	slow, slowDropped := newTestSubscriber(1)
	healthy, healthyDropped := newTestSubscriber(8)

	hub.Subscribe(slow, memoryDaily())
	hub.Subscribe(healthy, memoryDaily())

	// The slow subscriber never drains, so its single slot fills on the first frame.
	for range 4 {
		hub.MarkDirty(memoryDaily())

		done := make(chan struct{})
		go func() {
			hub.Flush(context.Background())
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Flush blocked on a subscriber that was not reading")
		}
	}

	require.True(t, slowDropped.Load(), "a subscriber that cannot keep up should be dropped")
	require.False(t, healthyDropped.Load(), "one slow client must not affect the others")
	require.Positive(t, len(healthy.send))
}

func TestDropIsOnlyReportedOnce(t *testing.T) {
	var drops atomic.Int32

	sub := &subscriber{send: make(chan []byte, 1), drop: func() { drops.Add(1) }}

	require.True(t, sub.deliver([]byte("first")))
	for range 5 {
		require.False(t, sub.deliver([]byte("overflow")))
	}

	require.Equal(t, int32(1), drops.Load(), "the drop callback should fire once, not per frame")
}

func TestSnapshotFailureDoesNotStopOtherChannels(t *testing.T) {
	boards := &fakeBoards{err: errors.New("redis is unreachable")}
	hub := NewHub(boards, testConfig(), discard())

	sub, dropped := newTestSubscriber(4)
	hub.Subscribe(sub, memoryDaily())
	hub.MarkDirty(memoryDaily())

	require.NotPanics(t, func() { hub.Flush(context.Background()) })

	require.Empty(t, sub.send, "a failed read should send nothing rather than a broken frame")
	require.False(t, dropped.Load(), "a Redis failure is not the client's fault")
}

func TestPublishSendsOneChannelToOneSubscriber(t *testing.T) {
	boards := &fakeBoards{page: leaderboard.Page{Total: 1}}
	hub := NewHub(boards, testConfig(), discard())

	sub, _ := newTestSubscriber(4)

	require.NoError(t, hub.Publish(context.Background(), sub, memoryDaily()))

	var snapshot Snapshot
	require.NoError(t, json.Unmarshal(<-sub.send, &snapshot))
	require.Equal(t, "memory:daily", snapshot.Channel)
}

func TestRunFlushesOnItsTickerUntilCancelled(t *testing.T) {
	boards := &fakeBoards{}
	hub := NewHub(boards, testConfig(), discard())

	sub, _ := newTestSubscriber(64)
	hub.Subscribe(sub, memoryDaily())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hub.Run(ctx) }()

	require.Eventually(t, func() bool {
		hub.MarkDirty(memoryDaily())
		return boards.readCount() > 0
	}, 2*time.Second, 5*time.Millisecond)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// The hub is written to concurrently by the bridge, by every connection subscribing, and by its
// own flush loop. Run under -race in CI.
func TestHubIsSafeUnderConcurrentUse(t *testing.T) {
	hub := NewHub(&fakeBoards{}, testConfig(), discard())

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			sub, _ := newTestSubscriber(8)
			for range 20 {
				hub.Subscribe(sub, memoryDaily())
				hub.MarkDirty(memoryDaily())
				hub.Flush(context.Background())
				hub.Subscribers(memoryDaily())
				hub.Unsubscribe(sub, memoryDaily())
			}
			hub.Remove(sub)
		}()
	}
	wg.Wait()

	require.Zero(t, hub.Subscribers(memoryDaily()))
}
