package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
	})
}

func TestRequestIDGeneratedAndEchoed(t *testing.T) {
	rec := do(t, RequestID(okHandler()), http.MethodGet, "/x", "")
	require.Len(t, rec.Header().Get(RequestIDHeader), 16)
}

func TestRequestIDReusesInboundValue(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set(RequestIDHeader, "trace-from-proxy")

	rec := httptest.NewRecorder()
	RequestID(okHandler()).ServeHTTP(rec, r)

	require.Equal(t, "trace-from-proxy", rec.Header().Get(RequestIDHeader))
}

func TestRequestIDRejectsOverlongInboundValue(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set(RequestIDHeader, strings.Repeat("x", 65))

	rec := httptest.NewRecorder()
	RequestID(okHandler()).ServeHTTP(rec, r)

	require.Len(t, rec.Header().Get(RequestIDHeader), 16)
}

func TestLoggerRecordsCompletedRequest(t *testing.T) {
	rec := do(t, Logger(discardLogger())(okHandler()), http.MethodGet, "/x", "")
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestRecovererTurnsPanicIntoInternalError(t *testing.T) {
	h := RequestID(Logger(discardLogger())(Recoverer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			panic("boom")
		}))))

	rec := do(t, h, http.MethodGet, "/x", "")

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "internal", decodeError(t, rec).Code)
	require.NotContains(t, rec.Body.String(), "boom")
}

func TestRecovererRepanicsOnAbortHandler(t *testing.T) {
	h := Recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	require.Panics(t, func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	})
}

func TestMaxBodyRejectsOversizedRequest(t *testing.T) {
	var got payload
	h := MaxBody(16)(decodeHandler(&got))

	rec := do(t, h, http.MethodPost, "/x", `{"name":"`+strings.Repeat("a", 200)+`"}`)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, decodeError(t, rec).Message, "too large")
}
