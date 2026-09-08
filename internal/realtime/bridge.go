package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/leaderboard"
	"github.com/saim61/podium/internal/platform/redis"
)

// notice is the message that crosses Pub/Sub. It says which board moved, nothing more.
type notice struct {
	Scope  string             `json:"scope"`
	Period leaderboard.Period `json:"period"`
}

// Publisher announces leaderboard changes to every instance.
type Publisher struct {
	client  *redis.Client
	timeout time.Duration
}

// NewPublisher builds the publishing side.
func NewPublisher(client *redis.Client, timeout time.Duration) *Publisher {
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	return &Publisher{client: client, timeout: timeout}
}

// Publish announces that one board changed.
func (p *Publisher) Publish(ctx context.Context, scope string, period leaderboard.Period) error {
	payload, err := json.Marshal(notice{Scope: scope, Period: period})
	if err != nil {
		return fmt.Errorf("encode leaderboard notice: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	if err := p.client.Publish(ctx, UpdateChannel, payload).Err(); err != nil {
		return fmt.Errorf("publish leaderboard notice: %w", err)
	}
	return nil
}

// Bridge feeds Pub/Sub notices into the local hub.
//
// This is the piece that makes the design survive more than one instance. A hub broadcasting
// only what its own process scored works perfectly on one machine and silently breaks on two:
// a score submitted to instance A would never reach a socket held by instance B. Every instance
// listens to every change, and serves the ones its own clients asked for.
type Bridge struct {
	client   *redis.Client
	hub      *Hub
	registry *games.Registry
	log      *slog.Logger
}

// NewBridge builds the subscribing side.
func NewBridge(client *redis.Client, hub *Hub, registry *games.Registry, log *slog.Logger) *Bridge {
	return &Bridge{client: client, hub: hub, registry: registry, log: log}
}

// Run subscribes until the context is cancelled.
func (b *Bridge) Run(ctx context.Context) error {
	sub := b.client.Subscribe(ctx, UpdateChannel)
	defer func() { _ = sub.Close() }()

	// Confirms the subscription is live before returning, so a caller that starts the bridge
	// and immediately publishes does not race it.
	if _, err := sub.Receive(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("subscribe to %s: %w", UpdateChannel, err)
	}

	b.log.Info("realtime bridge subscribed", slog.String("channel", UpdateChannel))

	messages := sub.Channel()
	for {
		select {
		case message, ok := <-messages:
			if !ok {
				return nil
			}
			b.handle(message.Payload)

		case <-ctx.Done():
			b.log.Info("realtime bridge stopped")
			return nil
		}
	}
}

func (b *Bridge) handle(payload string) {
	var received notice
	if err := json.Unmarshal([]byte(payload), &received); err != nil {
		b.log.Warn("discarded malformed leaderboard notice", slog.Any("error", err))
		return
	}

	channel, err := ParseChannel(received.Scope+":"+string(received.Period), b.registry)
	if err != nil {
		b.log.Warn("discarded notice for an unknown board", slog.Any("error", err))
		return
	}
	b.hub.MarkDirty(channel)
}
