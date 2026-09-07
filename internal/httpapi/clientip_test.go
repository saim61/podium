package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func requestFrom(remoteAddr string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.RemoteAddr = remoteAddr
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	return r
}

func TestClientIPUsesRemoteAddrByDefault(t *testing.T) {
	r := requestFrom("203.0.113.7:54321", nil)

	require.Equal(t, "203.0.113.7", ClientIP(r, false))
}

func TestClientIPIgnoresForwardedHeadersWhenUntrusted(t *testing.T) {
	r := requestFrom("203.0.113.7:54321", map[string]string{
		"X-Forwarded-For": "198.51.100.1",
		"X-Real-Ip":       "198.51.100.2",
	})

	require.Equal(t, "203.0.113.7", ClientIP(r, false),
		"a spoofable header must not override the real peer address")
}

func TestClientIPUsesLeftmostForwardedAddressWhenTrusted(t *testing.T) {
	r := requestFrom("10.0.0.1:443", map[string]string{
		"X-Forwarded-For": "198.51.100.1, 10.0.0.9, 10.0.0.1",
	})

	require.Equal(t, "198.51.100.1", ClientIP(r, true))
}

func TestClientIPFallsBackToRealIPHeader(t *testing.T) {
	r := requestFrom("10.0.0.1:443", map[string]string{"X-Real-Ip": "198.51.100.5"})

	require.Equal(t, "198.51.100.5", ClientIP(r, true))
}

func TestClientIPFallsBackWhenForwardedHeaderIsGarbage(t *testing.T) {
	r := requestFrom("203.0.113.7:54321", map[string]string{"X-Forwarded-For": "not-an-ip"})

	require.Equal(t, "203.0.113.7", ClientIP(r, true))
}

func TestClientIPHandlesAddressWithoutPort(t *testing.T) {
	r := requestFrom("203.0.113.7", nil)

	require.Equal(t, "203.0.113.7", ClientIP(r, false))
}

func TestClientIPHandlesIPv6(t *testing.T) {
	r := requestFrom("[2001:db8::1]:54321", nil)

	require.Equal(t, "2001:db8::1", ClientIP(r, false))
}
