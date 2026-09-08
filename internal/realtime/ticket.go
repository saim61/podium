package realtime

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/platform/redis"
)

// ErrTicketInvalid means a handshake presented a ticket that was never issued, has expired, or
// has already been spent.
var ErrTicketInvalid = errors.New("realtime ticket is not valid")

// Tickets issues and redeems short-lived, single-use credentials for a WebSocket handshake.
//
// A browser cannot set an Authorization header on a WebSocket handshake, and putting a JWT in
// the query string writes it into every access log and proxy trace along the way. A ticket is
// the usual answer: it is worthless thirty seconds later and worthless a second time.
type Tickets struct {
	client  *redis.Client
	ttl     time.Duration
	timeout time.Duration
}

// NewTickets builds the ticket store.
func NewTickets(client *redis.Client, cfg config.Realtime, timeout time.Duration) *Tickets {
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	return &Tickets{client: client, ttl: cfg.TicketTTL, timeout: timeout}
}

// TTL is how long an issued ticket stays usable.
func (t *Tickets) TTL() time.Duration { return t.ttl }

func ticketKey(token string) string { return "rt:ticket:" + token }

// Issue mints a ticket for a user.
func (t *Tickets) Issue(ctx context.Context, userID int64) (string, error) {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate realtime ticket: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf[:])

	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	if err := t.client.Set(ctx, ticketKey(token), userID, t.ttl).Err(); err != nil {
		return "", fmt.Errorf("store realtime ticket: %w", err)
	}
	return token, nil
}

// Redeem spends a ticket and returns whose it was.
//
// GETDEL, so redemption is atomic: two handshakes racing on one stolen ticket cannot both
// succeed. A ticket read and then deleted in two commands would let both through.
func (t *Tickets) Redeem(ctx context.Context, token string) (int64, error) {
	if token == "" {
		return 0, ErrTicketInvalid
	}

	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	raw, err := t.client.GetDel(ctx, ticketKey(token)).Result()
	if err != nil {
		if errors.Is(err, goredis.Nil) {
			return 0, ErrTicketInvalid
		}
		return 0, fmt.Errorf("redeem realtime ticket: %w", err)
	}

	userID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, ErrTicketInvalid
	}
	return userID, nil
}
