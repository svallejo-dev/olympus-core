package atlas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// statusClientClosedRequest is the de facto status for a client that left before the response.
const statusClientClosedRequest = 499

// statusCoder is what a handler's error implements to choose its status, without importing atlas.
type statusCoder interface {
	error
	StatusCode() int
}

type statusError struct {
	status int
	err    error
}

func (e *statusError) Error() string   { return e.err.Error() }
func (e *statusError) Unwrap() error   { return e.err }
func (e *statusError) StatusCode() int { return e.status }

// Errorf returns an error with an HTTP status; below 500 its message reaches the client, so it must not carry internals.
func Errorf(status int, format string, args ...any) error {
	return &statusError{status: status, err: fmt.Errorf(format, args...)}
}

func statusOf(err error) int {
	if coder, ok := errors.AsType[statusCoder](err); ok {
		if code := coder.StatusCode(); code >= http.StatusBadRequest && code <= 599 {
			return code
		}
	}
	return http.StatusInternalServerError
}

func (t *routeTable) adapt(h HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorderFor(w)
		if err := h(rec, r); err != nil {
			t.fail(rec, r, err)
		}
	})
}

func (t *routeTable) fail(w *responseRecorder, r *http.Request, err error) {
	ctx := r.Context()
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		if w.status == 0 {
			w.WriteHeader(statusClientClosedRequest)
		}
		return
	}
	started := w.status != 0
	if started || statusOf(err) >= http.StatusInternalServerError {
		t.logger.LogAttrs(ctx, slog.LevelError, "request failed",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("err", err.Error()),
		)
	}
	if started {
		// Headers are gone; aborting makes the client see a broken response, not a truncated one that looks complete.
		panic(http.ErrAbortHandler)
	}
	t.writeError(w, r, err)
}

// problem is an RFC 9457 error body.
type problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

func writeProblem(w http.ResponseWriter, r *http.Request, err error) {
	status := statusOf(err)
	p := problem{Type: "about:blank", Title: http.StatusText(status), Status: status, RequestID: RequestID(r.Context())}
	if coder, ok := errors.AsType[statusCoder](err); ok && status < http.StatusInternalServerError {
		p.Detail = coder.Error()
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Del("Content-Length")
	w.WriteHeader(status)
	// A failed write means the client went away; there is nobody left to tell.
	_ = json.NewEncoder(w).Encode(p)
}
