package integration

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDemoPageIsServed(t *testing.T) {
	h := newAuthHarness(t)

	rec := h.do(t, http.MethodGet, "/", nil, "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "<title>Podium</title>")
	require.Contains(t, rec.Header().Get("Content-Type"), "text/html")
}

func TestDemoAssetsAreServed(t *testing.T) {
	h := newAuthHarness(t)

	for path, wants := range map[string]string{
		"/app.js":    "javascript",
		"/style.css": "css",
	} {
		t.Run(path, func(t *testing.T) {
			rec := h.do(t, http.MethodGet, path, nil, "")

			require.Equal(t, http.StatusOK, rec.Code)
			require.Contains(t, rec.Header().Get("Content-Type"), wants)
			require.NotEmpty(t, rec.Body.String())
		})
	}
}

// The page is registered on explicit paths rather than a catch-all at the root. A catch-all would
// also answer an unknown /v1 path, so a client that mistyped an endpoint would receive HTML
// instead of the JSON error envelope the API promises everywhere else.
func TestServingThePageDoesNotShadowTheAPI(t *testing.T) {
	h := newAuthHarness(t)

	for _, path := range []string{"/v1/nope", "/v1/games/pinball", "/healthz/extra", "/nonsense"} {
		t.Run(path, func(t *testing.T) {
			rec := h.do(t, http.MethodGet, path, nil, "")

			require.NotContains(t, rec.Body.String(), "<html",
				"an unknown path must not be answered with the demo page")

			if strings.HasPrefix(path, "/v1") {
				require.Equal(t, http.StatusNotFound, rec.Code)
				require.Equal(t, "not_found", decodeInto[errBody](t, rec).Error.Code)
			}
		})
	}
}

// The demo page must not need a build step: no bundler output, no imports to resolve.
func TestDemoPageHasNoBuildStep(t *testing.T) {
	h := newAuthHarness(t)

	page := h.do(t, http.MethodGet, "/", nil, "").Body.String()

	require.NotContains(t, page, "type=\"module\"")
	require.NotContains(t, page, "node_modules")
	require.Contains(t, page, `src="/app.js"`)
	require.Contains(t, page, `href="/style.css"`)

	script := h.do(t, http.MethodGet, "/app.js", nil, "").Body.String()
	require.NotContains(t, script, "import ")
	require.NotContains(t, script, "require(")
}
