package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestErrorConstructors(t *testing.T) {
	for _, tc := range []struct {
		err        *Error
		wantStatus int
		wantCode   string
	}{
		{BadRequest("x"), http.StatusBadRequest, "bad_request"},
		{Unauthorized("x"), http.StatusUnauthorized, "unauthorized"},
		{Forbidden("x"), http.StatusForbidden, "forbidden"},
		{NotFound("x"), http.StatusNotFound, "not_found"},
		{Conflict("x"), http.StatusConflict, "conflict"},
		{TooManyRequests("x"), http.StatusTooManyRequests, "rate_limited"},
		{Unavailable("x"), http.StatusServiceUnavailable, "unavailable"},
	} {
		require.Equal(t, tc.wantStatus, tc.err.Status, tc.wantCode)
		require.Equal(t, tc.wantCode, tc.err.Code)
	}
}

func TestErrorUnwrapsCause(t *testing.T) {
	cause := errors.New("underlying")
	err := Conflict("nope").Because(cause)

	require.ErrorIs(t, err, cause)
	require.Contains(t, err.Error(), "underlying")
}

func TestWriteErrorRendersEnvelope(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, BadRequest("username is taken").
			WithFields(map[string]string{"username": "already registered"}))
	}))

	rec := do(t, h, http.MethodPost, "/x", "")

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))

	body := decodeError(t, rec)
	require.Equal(t, "bad_request", body.Code)
	require.Equal(t, "username is taken", body.Message)
	require.Equal(t, "already registered", body.Fields["username"])
	require.NotEmpty(t, body.RequestID)
}

func TestWriteErrorHidesUnexpectedCause(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, errors.New("connection string is postgres://user:hunter2@db"))
	})

	rec := do(t, h, http.MethodGet, "/x", "")

	require.Equal(t, http.StatusInternalServerError, rec.Code)

	body := decodeError(t, rec)
	require.Equal(t, "internal", body.Code)
	require.Equal(t, "an unexpected error occurred", body.Message)
	require.NotContains(t, rec.Body.String(), "hunter2")
}

func TestWriteErrorFindsWrappedAPIError(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, fmt.Errorf("loading session: %w", NotFound("no such session")))
	})

	rec := do(t, h, http.MethodGet, "/x", "")

	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "not_found", decodeError(t, rec).Code)
}

func TestDecodeJSON(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		wantStatus int
		wantMsg    string
	}{
		{"valid", `{"name":"saeem"}`, http.StatusOK, ""},
		{"unknown field", `{"name":"a","admin":true}`, http.StatusBadRequest, "not valid JSON"},
		{"malformed", `{"name":`, http.StatusBadRequest, "not valid JSON"},
		{"two objects", `{"name":"a"} {"name":"b"}`, http.StatusBadRequest, "single JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got payload
			rec := do(t, decodeHandler(&got), http.MethodPost, "/x", tc.body)

			require.Equal(t, tc.wantStatus, rec.Code)
			if tc.wantMsg != "" {
				require.Contains(t, decodeError(t, rec).Message, tc.wantMsg)
			}
		})
	}
}
