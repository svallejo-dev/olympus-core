package atlas

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"
)

const (
	requestIDHeader    = "X-Request-ID"
	maxRequestIDLength = 128
)

type requestIDKey struct{}

// RequestID returns the request ID that Run assigned to the request carrying ctx, or "" if none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID(id) {
			id = rand.Text()
		}
		w.Header().Set(requestIDHeader, id)

		req := r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
		next.ServeHTTP(w, req)
		// otelhttp reads the route matched by an inner ServeMux from the request it passed down.
		r.Pattern = req.Pattern
		nameSpan(req)
	})
}

// nameSpan follows the OpenTelemetry convention "METHOD /route", known only once a ServeMux has matched.
func nameSpan(r *http.Request) {
	switch {
	case r.Pattern == "":
	case strings.Contains(r.Pattern, " "):
		trace.SpanFromContext(r.Context()).SetName(r.Pattern)
	default:
		trace.SpanFromContext(r.Context()).SetName(r.Method + " " + r.Pattern)
	}
}

func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLength {
		return false
	}
	for _, c := range []byte(id) {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := recorderFor(w)
		aborted := true
		// Deferred so a request aborted by a panic (http.ErrAbortHandler) is logged before the panic goes on.
		defer func() { logRequest(logger, r, rec, time.Since(start), aborted) }()
		next.ServeHTTP(rec, r)
		aborted = false
	})
}

func logRequest(logger *slog.Logger, r *http.Request, rec *responseRecorder, elapsed time.Duration, aborted bool) {
	status := rec.status
	switch {
	case status == 0 && aborted:
		status = http.StatusInternalServerError
	case status == 0:
		status = http.StatusOK
	}
	level := slog.LevelInfo
	if status >= http.StatusInternalServerError || aborted {
		level = slog.LevelError
	}
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Int64("bytes", rec.bytes),
		slog.Duration("duration", elapsed),
	}
	if aborted {
		attrs = append(attrs, slog.Bool("aborted", true))
	}
	logger.LogAttrs(r.Context(), level, "request", attrs...)
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

// recorderFor reuses w when an outer middleware already wraps it, so every layer sees one status.
func recorderFor(w http.ResponseWriter) *responseRecorder {
	if rec, ok := w.(*responseRecorder); ok {
		return rec
	}
	return &responseRecorder{ResponseWriter: w}
}

func (rec *responseRecorder) WriteHeader(code int) {
	// 1xx responses such as 103 Early Hints are informational; the final status comes later.
	if rec.status == 0 && code >= http.StatusOK {
		rec.status = code
	}
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *responseRecorder) Write(p []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	n, err := rec.ResponseWriter.Write(p)
	rec.bytes += int64(n)
	return n, err
}

// ReadFrom keeps the sendfile fast path of http.ServeContent and io.Copy, which a plain Write loop would lose.
func (rec *responseRecorder) ReadFrom(src io.Reader) (int64, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	n, err := io.Copy(rec.ResponseWriter, src)
	rec.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer for deadlines and other extensions.
func (rec *responseRecorder) Unwrap() http.ResponseWriter {
	return rec.ResponseWriter
}

// Flush exists because frameworks such as Gin assert http.Flusher instead of using http.ResponseController.
func (rec *responseRecorder) Flush() {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	_ = http.NewResponseController(rec.ResponseWriter).Flush()
}

// Hijack exists for the same reason as Flush; it fails on connections that cannot be hijacked.
func (rec *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(rec.ResponseWriter).Hijack()
	if err == nil && rec.status == 0 {
		rec.status = http.StatusSwitchingProtocols
	}
	return conn, rw, err
}

// contextHandler adds the request and trace IDs of the record's context, so logs from handlers can be correlated without calling atlas.
type contextHandler struct {
	slog.Handler
}

func (h contextHandler) Handle(ctx context.Context, record slog.Record) error {
	record = record.Clone()
	if id := RequestID(ctx); id != "" {
		record.AddAttrs(slog.String("request_id", id))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		record.AddAttrs(slog.String("trace_id", sc.TraceID().String()))
	}
	return h.Handler.Handle(ctx, record)
}

// writesThroughLog reports slog's initial handler, which writes through package log.
func (h contextHandler) writesThroughLog() bool {
	return fmt.Sprintf("%T", h.Handler) == "*slog.defaultHandler"
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}
