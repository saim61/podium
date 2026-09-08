package httpapi

import (
	"net/http"

	"github.com/saim61/podium/internal/auth"
	"github.com/saim61/podium/internal/realtime"
)

type ticketResponse struct {
	Ticket    string `json:"ticket"`
	ExpiresIn int    `json:"expires_in"`
	URL       string `json:"url"`
}

type realtimeHandler struct {
	tickets *realtime.Tickets
}

// handleTicket mints the single-use credential a WebSocket handshake spends.
func (h *realtimeHandler) handleTicket(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFrom(r.Context())
	if !ok {
		WriteError(w, r, Unauthorized("authentication is required"))
		return
	}

	ticket, err := h.tickets.Issue(r.Context(), userID)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	WriteJSON(w, r, http.StatusCreated, ticketResponse{
		Ticket:    ticket,
		ExpiresIn: int(h.tickets.TTL().Seconds()),
		URL:       "/v1/ws?ticket=" + ticket,
	})
}
