package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/leaderboard"
)

// Boards is the read side of the leaderboards a snapshot comes from.
type Boards interface {
	Page(ctx context.Context, scope leaderboard.Scope, period leaderboard.Period, offset, limit int) (leaderboard.Page, error)
}

// Snapshot is what a subscriber receives.
type Snapshot struct {
	Type    string              `json:"type"`
	Channel string              `json:"channel"`
	Total   int64               `json:"total"`
	Entries []leaderboard.Entry `json:"entries"`
	At      time.Time           `json:"at"`
}

// subscriber is one connection as the hub sees it. The hub never touches a socket; it hands
// bytes to a buffered channel that the connection's own write pump drains.
type subscriber struct {
	send  chan []byte
	drop  func()
	close sync.Once
}

// deliver hands a frame to a subscriber, reporting whether it kept up.
//
// A non-blocking send is the whole point. If this blocked on a slow socket it would stall the
// flush goroutine and with it every other subscriber on every other channel - one bad client
// freezing the fan-out for everybody. A subscriber that cannot keep up is dropped instead.
func (s *subscriber) deliver(frame []byte) bool {
	select {
	case s.send <- frame:
		return true
	default:
		s.close.Do(s.drop)
		return false
	}
}

// Hub tracks who is subscribed to what and broadcasts snapshots.
type Hub struct {
	boards Boards
	cfg    config.Realtime
	log    *slog.Logger
	now    func() time.Time

	mu       sync.Mutex
	channels map[string]map[*subscriber]struct{}
	dirty    map[string]Channel
}

// NewHub builds a hub.
func NewHub(boards Boards, cfg config.Realtime, log *slog.Logger) *Hub {
	return &Hub{
		boards:   boards,
		cfg:      cfg,
		log:      log,
		now:      time.Now,
		channels: map[string]map[*subscriber]struct{}{},
		dirty:    map[string]Channel{},
	}
}

// Subscribe adds a subscriber to a channel.
func (h *Hub) Subscribe(sub *subscriber, channel Channel) {
	name := channel.String()

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.channels[name] == nil {
		h.channels[name] = map[*subscriber]struct{}{}
	}
	h.channels[name][sub] = struct{}{}
}

// Unsubscribe removes a subscriber from a channel.
func (h *Hub) Unsubscribe(sub *subscriber, channel Channel) {
	name := channel.String()

	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.channels[name], sub)
	if len(h.channels[name]) == 0 {
		delete(h.channels, name)
	}
}

// Remove drops a subscriber from every channel.
func (h *Hub) Remove(sub *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for name, subs := range h.channels {
		delete(subs, sub)
		if len(subs) == 0 {
			delete(h.channels, name)
		}
	}
}

// Subscribers reports how many connections are listening to a channel.
func (h *Hub) Subscribers(channel Channel) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return len(h.channels[channel.String()])
}

// MarkDirty notes that a board changed and should be re-sent on the next flush.
//
// Coalescing happens here. A busy game can be scored many times a second, and sending a frame
// per score would have clients rendering work nobody asked for. Marking is idempotent, so a
// hundred changes inside one flush window cost one read and one broadcast.
func (h *Hub) MarkDirty(channel Channel) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.dirty[channel.String()] = channel
}

// Run flushes on a ticker until the context is cancelled.
func (h *Hub) Run(ctx context.Context) error {
	ticker := time.NewTicker(h.cfg.FlushInterval)
	defer ticker.Stop()

	h.log.Info("realtime hub started",
		slog.String("flush_interval", h.cfg.FlushInterval.String()),
		slog.Int("top_n", h.cfg.TopN))

	for {
		select {
		case <-ticker.C:
			h.Flush(ctx)
		case <-ctx.Done():
			h.log.Info("realtime hub stopped")
			return nil
		}
	}
}

// Flush sends a fresh snapshot of every changed channel that anybody is listening to.
func (h *Hub) Flush(ctx context.Context) {
	for _, channel := range h.takeDirty() {
		if h.Subscribers(channel) == 0 {
			// Nobody is listening, so there is nothing to read Redis for.
			continue
		}

		frame, err := h.snapshot(ctx, channel)
		if err != nil {
			h.log.Error("could not build leaderboard snapshot",
				slog.String("channel", channel.String()),
				slog.Any("error", err))
			continue
		}
		h.broadcast(channel, frame)
	}
}

// Publish sends a snapshot of one channel to one subscriber, which is how a fresh subscription
// gets something to render without waiting for the next change.
func (h *Hub) Publish(ctx context.Context, sub *subscriber, channel Channel) error {
	frame, err := h.snapshot(ctx, channel)
	if err != nil {
		return err
	}

	sub.deliver(frame)
	return nil
}

func (h *Hub) takeDirty() []Channel {
	h.mu.Lock()
	defer h.mu.Unlock()

	channels := make([]Channel, 0, len(h.dirty))
	for _, channel := range h.dirty {
		channels = append(channels, channel)
	}
	h.dirty = map[string]Channel{}

	return channels
}

// snapshot reads a board and encodes it once. One marshal per channel per flush, shared by every
// subscriber on it - which is why snapshots carry public board data only and nothing per-user.
func (h *Hub) snapshot(ctx context.Context, channel Channel) ([]byte, error) {
	page, err := h.boards.Page(ctx, channel.Scope, channel.Period, 0, h.cfg.TopN)
	if err != nil {
		return nil, err
	}

	frame, err := json.Marshal(Snapshot{
		Type:    "snapshot",
		Channel: channel.String(),
		Total:   page.Total,
		Entries: page.Entries,
		At:      h.now(),
	})
	if err != nil {
		return nil, fmt.Errorf("encode snapshot: %w", err)
	}
	return frame, nil
}

func (h *Hub) broadcast(channel Channel, frame []byte) {
	h.mu.Lock()
	subs := make([]*subscriber, 0, len(h.channels[channel.String()]))
	for sub := range h.channels[channel.String()] {
		subs = append(subs, sub)
	}
	h.mu.Unlock()

	dropped := 0
	for _, sub := range subs {
		if !sub.deliver(frame) {
			dropped++
		}
	}

	if dropped > 0 {
		h.log.Warn("dropped subscribers that could not keep up",
			slog.String("channel", channel.String()),
			slog.Int("count", dropped))
	}
}
