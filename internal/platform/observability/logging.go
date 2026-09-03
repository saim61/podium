// Package observability provides Podium's structured logging and the request-scoped context
// values that carry a logger and request id through a handler chain.
package observability

import (
	"io"
	"log/slog"

	"github.com/saim61/podium/internal/config"
)

// NewLogger builds the process logger. Text output is for humans reading a terminal; JSON is for
// anything that ships logs somewhere.
func NewLogger(cfg config.Log, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.Level}

	var h slog.Handler
	if cfg.Format == config.FormatText {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}
