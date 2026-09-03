package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyLogger
)

// NewRequestID returns a random identifier for one request.
func NewRequestID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// WithRequestID stores a request id on the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}

// RequestID returns the context's request id, or "" if there is none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(keyRequestID).(string)
	return id
}

// WithLogger stores a logger on the context.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, keyLogger, log)
}

// Logger returns the context's logger, falling back to the default so a caller that has lost its
// context still logs somewhere instead of panicking.
func Logger(ctx context.Context) *slog.Logger {
	if log, ok := ctx.Value(keyLogger).(*slog.Logger); ok {
		return log
	}
	return slog.Default()
}
