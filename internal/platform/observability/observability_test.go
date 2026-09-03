package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/saim61/podium/internal/config"
	"github.com/stretchr/testify/require"
)

func TestNewLoggerEmitsJSON(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(config.Log{Level: slog.LevelInfo, Format: config.FormatJSON}, &buf)

	log.Info("hello", "game", "memory")

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.Equal(t, "hello", line["msg"])
	require.Equal(t, "memory", line["game"])
}

func TestNewLoggerRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(config.Log{Level: slog.LevelWarn, Format: config.FormatJSON}, &buf)

	log.Info("dropped")
	require.Empty(t, buf.String())

	log.Warn("kept")
	require.Contains(t, buf.String(), "kept")
}

func TestNewLoggerEmitsText(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(config.Log{Level: slog.LevelInfo, Format: config.FormatText}, &buf)

	log.Info("hello")
	require.Contains(t, buf.String(), "msg=hello")
	require.NotContains(t, buf.String(), `"msg"`)
}

func TestRequestIDRoundTrip(t *testing.T) {
	id := NewRequestID()
	require.Len(t, id, 16)
	require.NotEqual(t, id, NewRequestID())

	ctx := WithRequestID(context.Background(), id)
	require.Equal(t, id, RequestID(ctx))
}

func TestRequestIDAbsent(t *testing.T) {
	require.Empty(t, RequestID(context.Background()))
}

func TestLoggerRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(config.Log{Level: slog.LevelInfo, Format: config.FormatJSON}, &buf)

	ctx := WithLogger(context.Background(), log)
	Logger(ctx).Info("from context")

	require.Contains(t, buf.String(), "from context")
}

func TestLoggerFallsBackToDefault(t *testing.T) {
	require.NotNil(t, Logger(context.Background()))
}
