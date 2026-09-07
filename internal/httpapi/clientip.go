package httpapi

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP returns the address to attribute a request to.
//
// Forwarded headers are only consulted when trustProxy is set, because any client can send them.
// Trusting them unconditionally would let an attacker defeat per-IP rate limiting by putting a
// fresh value in X-Forwarded-For on every request.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			// Leftmost is the original client; everything after it was added by each hop.
			if ip := net.ParseIP(strings.TrimSpace(strings.Split(forwarded, ",")[0])); ip != nil {
				return ip.String()
			}
		}
		if realIP := r.Header.Get("X-Real-Ip"); realIP != "" {
			if ip := net.ParseIP(strings.TrimSpace(realIP)); ip != nil {
				return ip.String()
			}
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
