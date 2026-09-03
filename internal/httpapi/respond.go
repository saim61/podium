// Package httpapi wires Podium's HTTP surface: the router, its middleware, the shared error
// envelope and the health probes.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/saim61/podium/internal/platform/observability"
)

// Error is an HTTP-aware error. Handlers and services return it so the status code and machine
// readable code travel with the failure instead of being reconstructed at the boundary.
type Error struct {
	Status  int
	Code    string
	Message string
	Fields  map[string]string
	cause   error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// Because attaches a cause. The cause is logged but never sent to the client.
func (e *Error) Because(err error) *Error {
	e.cause = err
	return e
}

// WithFields attaches per-field validation detail.
func (e *Error) WithFields(fields map[string]string) *Error {
	e.Fields = fields
	return e
}

func newError(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// BadRequest reports a malformed or invalid request.
func BadRequest(message string) *Error {
	return newError(http.StatusBadRequest, "bad_request", message)
}

// Unauthorized reports missing or invalid credentials.
func Unauthorized(message string) *Error {
	return newError(http.StatusUnauthorized, "unauthorized", message)
}

// Forbidden reports valid credentials that do not permit the action.
func Forbidden(message string) *Error {
	return newError(http.StatusForbidden, "forbidden", message)
}

// NotFound reports a missing resource.
func NotFound(message string) *Error {
	return newError(http.StatusNotFound, "not_found", message)
}

// Conflict reports a request that contradicts current state.
func Conflict(message string) *Error {
	return newError(http.StatusConflict, "conflict", message)
}

// TooManyRequests reports a rate limited caller.
func TooManyRequests(message string) *Error {
	return newError(http.StatusTooManyRequests, "rate_limited", message)
}

// Unavailable reports a dependency Podium needs but cannot reach.
func Unavailable(message string) *Error {
	return newError(http.StatusServiceUnavailable, "unavailable", message)
}

// Internal reports a bug. The message is deliberately fixed: anything specific about an
// unexpected failure belongs in the logs, not in a response body.
func Internal(err error) *Error {
	return newError(http.StatusInternalServerError, "internal", "an unexpected error occurred").
		Because(err)
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	Fields    map[string]string `json:"fields,omitempty"`
	RequestID string            `json:"request_id,omitempty"`
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		observability.Logger(r.Context()).Error("write response body", slog.Any("error", err))
	}
}

// WriteError renders err in the shared envelope. Anything that is not an *Error is treated as a
// bug: the client gets a generic 500 and the real cause is logged.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		apiErr = Internal(err)
	}

	log := observability.Logger(r.Context())
	attrs := []any{
		slog.String("code", apiErr.Code),
		slog.Int("status", apiErr.Status),
		slog.Any("error", err),
	}
	if apiErr.Status >= http.StatusInternalServerError {
		log.Error("request failed", attrs...)
	} else {
		log.Warn("request rejected", attrs...)
	}

	WriteJSON(w, r, apiErr.Status, errorEnvelope{Error: errorBody{
		Code:      apiErr.Code,
		Message:   apiErr.Message,
		Fields:    apiErr.Fields,
		RequestID: observability.RequestID(r.Context()),
	}})
}

// DecodeJSON reads a JSON request body into v, rejecting unknown fields so a typo in a client
// payload surfaces as an error instead of a silently ignored zero value.
func DecodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return BadRequest("a JSON request body is required")
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return BadRequest("request body is too large").Because(err)
		}
		return BadRequest("request body is not valid JSON").Because(err)
	}
	if dec.More() {
		return BadRequest("request body must contain a single JSON object")
	}
	return nil
}
