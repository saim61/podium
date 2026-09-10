package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/saim61/podium/internal/auth"
	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/ratelimit"
)

// RateLimit caps how often one caller may hit a group of routes.
//
// Keyed by account when the request is authenticated and by address otherwise. Keying reads by
// address alone would let one logged-in user behind a shared NAT exhaust the budget for everyone
// else on it; keying by account alone leaves anonymous traffic unmetered entirely.
func RateLimit(limiter *ratelimit.Limiter, cfg config.Auth, class string, limit int, window time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := "rl:" + class + ":"
			if userID, ok := auth.UserIDFrom(r.Context()); ok {
				key += "user:" + strconv.FormatInt(userID, 10)
			} else {
				key += "ip:" + ClientIP(r, cfg.TrustProxyIP)
			}

			if err := enforceLimit(r, w, limiter, key, limit, window); err != nil {
				WriteError(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
