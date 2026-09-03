package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()

	var env errorEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	return env.Error
}

type payload struct {
	Name string `json:"name"`
}

func decodeHandler(dst *payload) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := DecodeJSON(r, dst); err != nil {
			WriteError(w, r, err)
			return
		}
		WriteJSON(w, r, http.StatusOK, dst)
	}
}
