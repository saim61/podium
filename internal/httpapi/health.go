package httpapi

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// Check is one readiness probe against a dependency.
type Check struct {
	Name  string
	Probe func(context.Context) error
}

const readyTimeout = 3 * time.Second

type checkResult struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type healthResponse struct {
	Status string                 `json:"status"`
	Checks map[string]checkResult `json:"checks,omitempty"`
}

// handleLive answers liveness. It deliberately touches no dependency: a failing liveness probe
// restarts the process, and a brief Postgres blip must not restart every replica at once.
func handleLive() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, r, http.StatusOK, healthResponse{Status: "ok"})
	}
}

// handleReady answers readiness by probing every dependency in parallel. A failure returns 503,
// which pulls the instance out of the load balancer without killing it.
func handleReady(checks []Check, detail bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()

		results := make(map[string]checkResult, len(checks))
		var mu sync.Mutex
		var wg sync.WaitGroup

		for _, c := range checks {
			wg.Add(1)
			go func(c Check) {
				defer wg.Done()

				result := checkResult{Status: "ok"}
				if err := c.Probe(ctx); err != nil {
					result.Status = "error"
					if detail {
						result.Error = err.Error()
					}
				}

				mu.Lock()
				results[c.Name] = result
				mu.Unlock()
			}(c)
		}
		wg.Wait()

		status, body := http.StatusOK, healthResponse{Status: "ok", Checks: results}
		for _, result := range results {
			if result.Status != "ok" {
				status, body.Status = http.StatusServiceUnavailable, "unavailable"
				break
			}
		}
		WriteJSON(w, r, status, body)
	}
}
