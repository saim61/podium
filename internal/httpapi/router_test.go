package httpapi

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnknownRouteReturnsEnvelope(t *testing.T) {
	rec := do(t, testRouter(t), http.MethodGet, "/nope", "")

	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "not_found", decodeError(t, rec).Code)
}

func TestWrongMethodReturnsEnvelope(t *testing.T) {
	rec := do(t, testRouter(t), http.MethodDelete, "/healthz", "")

	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	require.Equal(t, "method_not_allowed", decodeError(t, rec).Code)
}
