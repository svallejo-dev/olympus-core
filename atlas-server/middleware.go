package atlas

import (
	"bufio"
	"context"
	"crypto/rand"
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
		// otelhttp reads the route matched by an inner ServeMux from the request it passed down.
		// Deferred so an aborted request keeps its route; {$} is routing syntax nobody should see in a metric.
		defer func() { r.Pattern = strings.TrimSuffix(req.Pattern, "{$}") }()
		next.ServeHTTP(w, req)
	})
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
		slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
	}
	if route := routeOf(r.Pattern); route != "" {
		attrs = append(attrs, slog.String("route", route))
	}
	if aborted {
		attrs = append(attrs, slog.Bool("aborted", true))
	}
	logger.LogAttrs(r.Context(), level, "request", attrs...)
}

// routeOf returns the path of a ServeMux pattern, without its method or the {$} routing syntax.
func routeOf(pattern string) string {
	_, path, found := strings.Cut(pattern, " ")
	if !found {
		path = pattern
	}
	return strings.TrimSuffix(path, "{$}")
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
	// 1xx responses such as 103 Early Hints are informational, but 101 ends the HTTP exchange.
	if rec.status == 0 && (code >= http.StatusOK || code == http.StatusSwitchingProtocols) {
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

// contextHandler adds the request, trace and span IDs of the record's context, so logs from handlers can be correlated without calling atlas.
type contextHandler struct {
	slog.Handler
}

func withContextIDs(h slog.Handler) slog.Handler {
	if _, ok := h.(contextHandler); ok {
		return h
	}
	return contextHandler{h}
}

func (h contextHandler) Handle(ctx context.Context, record slog.Record) error {
	record = record.Clone()
	if id := RequestID(ctx); id != "" {
		record.AddAttrs(slog.String("request_id", id))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		record.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, record)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}
