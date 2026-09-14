package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/platform/observability"
)

// clientMessage is what a client may send.
type clientMessage struct {
	Op       string   `json:"op"`
	Channels []string `json:"channels"`
}

type errorMessage struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type welcomeMessage struct {
	Type        string   `json:"type"`
	Subscribed  []string `json:"subscribed"`
	FlushMillis int      `json:"flush_ms"`
}

// Server upgrades HTTP requests to WebSocket connections and runs them.
type Server struct {
	hub      *Hub
	tickets  *Tickets
	registry *games.Registry
	cfg      config.Realtime
	log      *slog.Logger
	metrics  *observability.Metrics
}

// ServerOption adjusts a Server.
type ServerOption func(*Server)

// WithServerMetrics attaches the collectors connection counts go into.
func WithServerMetrics(m *observability.Metrics) ServerOption {
	return func(s *Server) { s.metrics = m }
}

// NewServer builds the WebSocket endpoint.
func NewServer(hub *Hub, tickets *Tickets, registry *games.Registry, cfg config.Realtime, log *slog.Logger, opts ...ServerOption) *Server {
	s := &Server{hub: hub, tickets: tickets, registry: registry, cfg: cfg, log: log}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Handler upgrades and serves one connection.
//
// A ticket is required even though the snapshots carry only public board data. A socket is a
// scarce resource in a way an HTTP read is not - it occupies memory and two goroutines for as
// long as it is held - so every one is tied to an account that limits can be applied to.
func (s *Server) Handler(w http.ResponseWriter, r *http.Request) {
	userID, err := s.tickets.Redeem(r.Context(), r.URL.Query().Get("ticket"))
	if err != nil {
		status := http.StatusUnauthorized
		if !errors.Is(err, ErrTicketInvalid) {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, `{"error":{"code":"unauthorized","message":"a valid realtime ticket is required"}}`, status)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Same-origin only by default; the demo page is served from this origin.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Warn("websocket upgrade failed", slog.Any("error", err))
		return
	}

	s.serve(r.Context(), conn, userID)
}

func (s *Server) serve(ctx context.Context, conn *websocket.Conn, userID int64) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sub := &subscriber{
		send: make(chan []byte, s.cfg.SendBuffer),
		drop: cancel,
	}
	defer s.hub.Remove(sub)

	if s.metrics != nil {
		s.metrics.RealtimeConnections.Inc()
		defer s.metrics.RealtimeConnections.Dec()
	}

	log := s.log.With(slog.Int64("user_id", userID))

	// Two goroutines per connection: one writing, one reading. They share nothing but the
	// context, so a stalled write cannot block a read and vice versa.
	go s.writePump(ctx, conn, sub, log)

	s.readPump(ctx, conn, sub, log)

	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// writePump is the only goroutine that writes to the socket. Concurrent writes to a WebSocket
// are not allowed, and funnelling them through one channel is what guarantees there is only one.
func (s *Server) writePump(ctx context.Context, conn *websocket.Conn, sub *subscriber, log *slog.Logger) {
	ping := time.NewTicker(s.cfg.PingInterval)
	defer ping.Stop()

	for {
		select {
		case frame := <-sub.send:
			writeCtx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
			err := conn.Write(writeCtx, websocket.MessageText, frame)
			cancel()

			if err != nil {
				log.Debug("websocket write failed, closing", slog.Any("error", err))
				_ = conn.Close(websocket.StatusInternalError, "write failed")
				return
			}

		case <-ping.C:
			pingCtx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
			err := conn.Ping(pingCtx)
			cancel()

			if err != nil {
				// A ping that goes unanswered is how a connection to a client that vanished
				// without closing gets noticed, instead of being held open forever.
				log.Debug("websocket ping failed, closing", slog.Any("error", err))
				_ = conn.Close(websocket.StatusPolicyViolation, "ping timeout")
				return
			}

		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) readPump(ctx context.Context, conn *websocket.Conn, sub *subscriber, log *slog.Logger) {
	subscribed := map[string]Channel{}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}

		var message clientMessage
		if err := json.Unmarshal(data, &message); err != nil {
			s.reject(sub, "message is not valid JSON")
			continue
		}

		switch message.Op {
		case "subscribe":
			s.handleSubscribe(ctx, sub, subscribed, message.Channels, log)
		case "unsubscribe":
			s.handleUnsubscribe(sub, subscribed, message.Channels)
		case "ping":
			sub.deliver([]byte(`{"type":"pong"}`))
		default:
			s.reject(sub, `op must be "subscribe", "unsubscribe" or "ping"`)
		}
	}
}

func (s *Server) handleSubscribe(
	ctx context.Context,
	sub *subscriber,
	subscribed map[string]Channel,
	names []string,
	log *slog.Logger,
) {
	for _, name := range names {
		if len(subscribed) >= s.cfg.MaxChannels {
			s.reject(sub, "too many channels on one connection")
			return
		}

		channel, err := ParseChannel(name, s.registry)
		if err != nil {
			s.reject(sub, err.Error())
			continue
		}
		if _, already := subscribed[channel.String()]; already {
			continue
		}

		subscribed[channel.String()] = channel
		s.hub.Subscribe(sub, channel)

		// Send the current board at once. Waiting for the next change would leave a client
		// with nothing to render on a quiet board.
		if err := s.hub.Publish(ctx, sub, channel); err != nil {
			log.Error("could not send opening snapshot",
				slog.String("channel", channel.String()), slog.Any("error", err))
		}
	}

	s.confirm(sub, subscribed)
}

func (s *Server) handleUnsubscribe(sub *subscriber, subscribed map[string]Channel, names []string) {
	for _, name := range names {
		channel, ok := subscribed[name]
		if !ok {
			continue
		}
		s.hub.Unsubscribe(sub, channel)
		delete(subscribed, name)
	}

	s.confirm(sub, subscribed)
}

func (s *Server) confirm(sub *subscriber, subscribed map[string]Channel) {
	names := make([]string, 0, len(subscribed))
	for name := range subscribed {
		names = append(names, name)
	}

	frame, err := json.Marshal(welcomeMessage{
		Type:        "subscribed",
		Subscribed:  names,
		FlushMillis: int(s.cfg.FlushInterval.Milliseconds()),
	})
	if err != nil {
		return
	}
	sub.deliver(frame)
}

func (s *Server) reject(sub *subscriber, message string) {
	frame, err := json.Marshal(errorMessage{Type: "error", Message: message})
	if err != nil {
		return
	}
	sub.deliver(frame)
}
