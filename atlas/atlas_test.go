package atlas_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/svallejo-dev/olympus/atlas"
)

const waitTimeout = 10 * time.Second

// logSink collects log output and wakes up a waiter on every write.
type logSink struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	notify chan struct{}
}

func newLogSink() *logSink {
	return &logSink{notify: make(chan struct{}, 1)}
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.buf.Write(p)
	select {
	case s.notify <- struct{}{}:
	default:
	}
	return n, err
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *logSink) waitFor(t *testing.T, substr string) {
	t.Helper()
	timeout := time.After(waitTimeout)
	for !strings.Contains(s.String(), substr) {
		select {
		case <-s.notify:
		case <-timeout:
			t.Fatalf("log never contained %q; got:\n%s", substr, s)
		}
	}
}

// records decodes every JSON log line whose msg equals msg.
func (s *logSink) records(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(s.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

type testService struct {
	url    string
	logs   *logSink
	cancel context.CancelFunc
	done   <-chan error
	wait   func() error // waits for Run to return on its own
	stop   func() error // cancels Run's context, then waits
}

// start runs atlas on a loopback listener, isolated from the global OpenTelemetry state.
func start(t *testing.T, handler http.Handler, opts ...atlas.Option) *testService {
	t.Helper()
	return startWith(t, handler, append([]atlas.Option{atlas.WithoutGlobalTelemetry()}, opts...)...)
}

func startWith(t *testing.T, handler http.Handler, opts ...atlas.Option) *testService {
	t.Helper()
	ln := listenLocal(t)
	logs := newLogSink()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	base := []atlas.Option{
		atlas.WithListener(ln),
		atlas.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))),
	}
	go func() { done <- atlas.Run(ctx, handler, append(base, opts...)...) }()

	svc := &testService{url: "http://" + ln.Addr().String(), logs: logs, cancel: cancel, done: done}
	svc.wait = sync.OnceValue(func() error {
		select {
		case err := <-done:
			return err
		case <-time.After(waitTimeout):
			return errors.New("Run did not return")
		}
	})
	svc.stop = func() error {
		cancel()
		return svc.wait()
	}
	t.Cleanup(func() { _ = svc.stop() })
	logs.waitFor(t, `"msg":"listening"`)
	return svc
}

func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

type response struct {
	status int
	header http.Header
	body   string
}

func get(t *testing.T, url string, header ...string) response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of %s: %v", url, err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: string(body)}
}

type healthReport struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func decodeReport(t *testing.T, body string) healthReport {
	t.Helper()
	var rep healthReport
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return rep
}

func helloMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello")
	})
	return mux
}

func TestRunServesAppAndOpsEndpoints(t *testing.T) {
	t.Parallel()
	svc := start(t, helloMux())

	if got := get(t, svc.url+"/hello"); got.status != http.StatusOK || got.body != "hello" {
		t.Errorf("GET /hello = %d %q, want 200 \"hello\"", got.status, got.body)
	}
	for _, path := range []string{"/livez", "/readyz", "/healthz", "/metrics"} {
		if got := get(t, svc.url+path); got.status != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, got.status)
		}
	}
	if err := svc.stop(); err != nil {
		t.Errorf("Run() = %v, want nil", err)
	}
	if len(svc.logs.records(t, "stopped")) != 1 {
		t.Errorf("missing stopped log:\n%s", svc.logs)
	}
}

func TestOpsEndpointWinsOverAppRoute(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	svc := start(t, mux)

	if got := get(t, svc.url+"/livez"); got.status != http.StatusOK {
		t.Errorf("GET /livez = %d, want 200 from atlas", got.status)
	}
	resp, err := http.Post(svc.url+"/livez", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST /livez: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("POST /livez = %d, want 418 from the app", resp.StatusCode)
	}
}

func TestReadinessChecks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		check      atlas.Check
		wantStatus int
		wantReady  healthReport
		wantHealth healthReport
	}{
		{
			name:       "passing",
			check:      func(context.Context) error { return nil },
			wantStatus: http.StatusOK,
			wantReady:  healthReport{Status: "ok", Checks: map[string]string{"db": "ok", "cache": "ok"}},
			wantHealth: healthReport{Status: "ok", Checks: map[string]string{"db": "ok", "cache": "ok"}},
		},
		{
			name:       "failing",
			check:      func(context.Context) error { return errors.New("dial tcp 10.0.0.7:5432: connection refused") },
			wantStatus: http.StatusServiceUnavailable,
			wantReady:  healthReport{Status: "unavailable", Checks: map[string]string{"db": "fail", "cache": "ok"}},
			wantHealth: healthReport{Status: "unavailable", Checks: map[string]string{"db": "error: dial tcp 10.0.0.7:5432: connection refused", "cache": "ok"}},
		},
		{
			name:       "panicking",
			check:      func(context.Context) error { panic("nil pool") },
			wantStatus: http.StatusServiceUnavailable,
			wantReady:  healthReport{Status: "unavailable", Checks: map[string]string{"db": "fail", "cache": "ok"}},
			wantHealth: healthReport{Status: "unavailable", Checks: map[string]string{"db": "error: panic: nil pool", "cache": "ok"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc := start(t, helloMux(),
				atlas.WithReadinessCheck("db", tt.check),
				atlas.WithReadinessCheck("cache", func(context.Context) error { return nil }),
			)
			for path, want := range map[string]healthReport{"/readyz": tt.wantReady, "/healthz": tt.wantHealth} {
				got := get(t, svc.url+path)
				if got.status != tt.wantStatus {
					t.Errorf("GET %s = %d, want %d", path, got.status, tt.wantStatus)
				}
				if ct := got.header.Get("Content-Type"); ct != "application/json" {
					t.Errorf("GET %s Content-Type = %q, want application/json", path, ct)
				}
				rep := decodeReport(t, got.body)
				if rep.Status != want.Status || !maps.Equal(rep.Checks, want.Checks) {
					t.Errorf("GET %s report = %+v, want %+v", path, rep, want)
				}
			}
		})
	}
}

func TestReadyzUnavailableDuringShutdownDelay(t *testing.T) {
	t.Parallel()
	// The delay only has to outlast three loopback requests.
	svc := start(t, helloMux(), atlas.WithShutdownDelay(time.Second))

	svc.cancel()
	svc.logs.waitFor(t, `"msg":"shutting down"`)

	got := get(t, svc.url+"/readyz")
	if got.status != http.StatusServiceUnavailable || decodeReport(t, got.body).Status != "unavailable" {
		t.Errorf("GET /readyz while draining = %d %s, want 503 unavailable", got.status, got.body)
	}
	if got := get(t, svc.url+"/livez"); got.status != http.StatusOK {
		t.Errorf("GET /livez while draining = %d, want 200", got.status)
	}
	if got := get(t, svc.url+"/hello"); got.status != http.StatusOK {
		t.Errorf("GET /hello while draining = %d, want 200", got.status)
	}
	if err := svc.stop(); err != nil {
		t.Errorf("Run() = %v, want nil", err)
	}
}

func TestInFlightRequestCompletesDuringShutdown(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	})
	svc := start(t, handler)

	type result struct {
		body string
		err  error
	}
	results := make(chan result, 1)
	go func() {
		resp, err := http.Get(svc.url + "/slow")
		if err != nil {
			results <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		results <- result{body: string(body), err: err}
	}()
	<-started

	svc.cancel()
	svc.logs.waitFor(t, `"msg":"shutting down"`)
	select {
	case err := <-svc.done:
		t.Fatalf("Run returned %v before the in-flight request finished", err)
	default:
	}

	close(release)
	if got := <-results; got.err != nil || got.body != "done" {
		t.Errorf("in-flight request = %q, %v; want \"done\"", got.body, got.err)
	}
	if err := svc.stop(); err != nil {
		t.Errorf("Run() = %v, want nil", err)
	}
}

func TestShutdownHooksRunInReverseOrder(t *testing.T) {
	t.Parallel()
	var order []string
	errCache := errors.New("cache close failed")
	errDB := errors.New("db close failed")
	hook := func(name string, err error) func(context.Context) error {
		return func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Errorf("hook %s got a context without deadline", name)
			}
			order = append(order, name)
			return err
		}
	}
	svc := start(t, helloMux(),
		atlas.WithShutdownHook("db", hook("db", errDB)),
		atlas.WithShutdownHook("queue", hook("queue", nil)),
		atlas.WithShutdownHook("cache", hook("cache", errCache)),
	)

	err := svc.stop()
	if want := []string{"cache", "queue", "db"}; !slices.Equal(order, want) {
		t.Errorf("hook order = %v, want %v", order, want)
	}
	if !errors.Is(err, errDB) || !errors.Is(err, errCache) {
		t.Errorf("Run() = %v, want both hook errors", err)
	}
}

func TestPanicIsRecovered(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	})
	svc := start(t, mux)

	got := get(t, svc.url+"/boom")
	if got.status != http.StatusInternalServerError {
		t.Errorf("GET /boom = %d, want 500", got.status)
	}
	if err := svc.stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	panics := svc.logs.records(t, "panic recovered")
	if len(panics) != 1 {
		t.Fatalf("got %d panic logs, want 1:\n%s", len(panics), svc.logs)
	}
	rec := panics[0]
	if rec["level"] != "ERROR" || rec["panic"] != "kaboom" {
		t.Errorf("panic log = %v, want ERROR with panic=kaboom", rec)
	}
	if id := got.header.Get("X-Request-ID"); id == "" || rec["request_id"] != id {
		t.Errorf("panic log request_id = %v, want response X-Request-ID %q", rec["request_id"], id)
	}
	if stack, _ := rec["stack"].(string); !strings.Contains(stack, "TestPanicIsRecovered") {
		t.Errorf("panic log stack does not point at the handler:\n%s", stack)
	}
	access := svc.logs.records(t, "request")
	if len(access) != 1 || access[0]["level"] != "ERROR" || access[0]["status"] != 500.0 {
		t.Errorf("access log = %v, want one ERROR entry with status 500", access)
	}
}

func TestPanicAfterWriteAbortsResponse(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "partial")
		_ = http.NewResponseController(w).Flush()
		panic("late failure")
	})
	svc := start(t, handler)

	resp, err := http.Get(svc.url + "/stream")
	if err != nil {
		t.Fatalf("GET /stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Error("reading the body succeeded, want an error from the aborted connection")
	}
	if err := svc.stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if len(svc.logs.records(t, "panic recovered")) != 1 {
		t.Errorf("want one panic log:\n%s", svc.logs)
	}
	access := svc.logs.records(t, "request")
	if len(access) != 1 || access[0]["level"] != "ERROR" || access[0]["status"] != 200.0 ||
		access[0]["bytes"] != 7.0 || access[0]["aborted"] != true {
		t.Errorf("access log = %v, want one ERROR entry with status 200, 7 bytes and aborted=true", access)
	}
}

func TestErrAbortHandlerIsRepanicked(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})
	svc := start(t, handler)

	if resp, err := http.Get(svc.url + "/abort"); err == nil {
		_ = resp.Body.Close()
		t.Errorf("GET /abort = %d, want the connection to be aborted", resp.StatusCode)
	}
	if err := svc.stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if logs := svc.logs.String(); strings.Contains(logs, "panic") {
		t.Errorf("ErrAbortHandler was logged as a crash:\n%s", logs)
	}
	access := svc.logs.records(t, "request")
	if len(access) != 1 || access[0]["status"] != 500.0 || access[0]["aborted"] != true {
		t.Errorf("access log = %v, want one entry with status 500 and aborted=true", access)
	}
}

func TestRequestID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		incoming string
		keep     bool
	}{
		{name: "generated", incoming: ""},
		{name: "propagated", incoming: "req-42_abc.def:1", keep: true},
		{name: "unsafe replaced", incoming: "<script>alert(1)</script>"},
		{name: "too long replaced", incoming: strings.Repeat("a", 129)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, atlas.RequestID(r.Context()))
			})
			svc := start(t, handler)

			var header []string
			if tt.incoming != "" {
				header = []string{"X-Request-ID", tt.incoming}
			}
			got := get(t, svc.url+"/", header...)
			id := got.header.Get("X-Request-ID")
			if id == "" || got.body != id {
				t.Fatalf("response header %q, handler saw %q; want the same non-empty ID", id, got.body)
			}
			if (id == tt.incoming) != tt.keep {
				t.Errorf("request ID = %q for incoming %q, keep = %v", id, tt.incoming, tt.keep)
			}
		})
	}
}

func TestRequestIDOutsideRequest(t *testing.T) {
	t.Parallel()
	if id := atlas.RequestID(t.Context()); id != "" {
		t.Errorf("RequestID(no request) = %q, want empty", id)
	}
}

func TestAccessLogIncludesTraceID(t *testing.T) {
	t.Parallel()
	svc := start(t, helloMux())

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	get(t, svc.url+"/hello", "traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	if err := svc.stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	access := svc.logs.records(t, "request")
	if len(access) != 1 {
		t.Fatalf("got %d access logs, want 1:\n%s", len(access), svc.logs)
	}
	rec := access[0]
	if rec["level"] != "INFO" || rec["method"] != "GET" || rec["path"] != "/hello" || rec["status"] != 200.0 ||
		rec["bytes"] != 5.0 || rec["trace_id"] != traceID || rec["request_id"] == "" {
		t.Errorf("access log = %v", rec)
	}
}

func TestWithoutAccessLog(t *testing.T) {
	t.Parallel()
	svc := start(t, helloMux(), atlas.WithoutAccessLog())
	get(t, svc.url+"/hello")
	if err := svc.stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if n := len(svc.logs.records(t, "request")); n != 0 {
		t.Errorf("got %d access logs, want 0", n)
	}
}

func TestMetricsExposeHTTPAndRuntimeMetrics(t *testing.T) {
	t.Parallel()
	svc := start(t, helloMux())

	get(t, svc.url+"/hello")
	metrics := get(t, svc.url+"/metrics").body
	for _, want := range []string{
		"http_server_request_duration_seconds_count{",
		`http_route="/hello"`,
		"go_goroutines ",
		"process_",
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
}

func TestGlobalTelemetryReachesMetricsEndpoint(t *testing.T) {
	svc := startWith(t, helloMux(), atlas.WithServiceName("orders"))

	counter, err := otel.GetMeterProvider().Meter("hermes").Int64Counter("hermes_test_calls")
	if err != nil {
		t.Fatalf("create counter: %v", err)
	}
	counter.Add(t.Context(), 3)

	metrics := get(t, svc.url+"/metrics").body
	if !strings.Contains(metrics, "hermes_test_calls_total") || !strings.Contains(metrics, `service_name="orders"`) {
		t.Errorf("/metrics lacks the global counter or service name:\n%s", metrics)
	}
	if fields := otel.GetTextMapPropagator().Fields(); !slices.Contains(fields, "traceparent") || !slices.Contains(fields, "baggage") {
		t.Errorf("global propagator fields = %v, want traceparent and baggage", fields)
	}
}

func TestAdminListenerSplitsOpsEndpoints(t *testing.T) {
	t.Parallel()
	adminLn := listenLocal(t)
	adminURL := "http://" + adminLn.Addr().String()
	mux := helloMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "app livez")
	})
	svc := start(t, mux, atlas.WithAdminListener(adminLn))

	if got := get(t, svc.url+"/hello"); got.status != http.StatusOK {
		t.Errorf("app GET /hello = %d, want 200", got.status)
	}
	if got := get(t, svc.url+"/livez"); got.body != "app livez" {
		t.Errorf("app GET /livez = %q, want the app's handler", got.body)
	}
	if got := get(t, svc.url+"/metrics"); got.status != http.StatusNotFound {
		t.Errorf("app GET /metrics = %d, want 404", got.status)
	}
	for _, path := range []string{"/livez", "/readyz", "/healthz", "/metrics"} {
		if got := get(t, adminURL+path); got.status != http.StatusOK {
			t.Errorf("admin GET %s = %d, want 200", path, got.status)
		}
	}
	if got := get(t, adminURL+"/hello"); got.status != http.StatusNotFound {
		t.Errorf("admin GET /hello = %d, want 404", got.status)
	}
	if logs := svc.logs.records(t, "listening"); len(logs) != 1 || logs[0]["admin_addr"] != adminLn.Addr().String() {
		t.Errorf("listening log = %v, want admin_addr %s", logs, adminLn.Addr())
	}
}

func TestRunReturnsListenError(t *testing.T) {
	t.Parallel()
	taken := listenLocal(t)
	defer func() { _ = taken.Close() }()

	err := atlas.Run(t.Context(), helloMux(),
		atlas.WithAddr(taken.Addr().String()),
		atlas.WithLogger(slog.New(slog.DiscardHandler)),
		atlas.WithoutGlobalTelemetry(),
	)
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("Run() = %v, want a listen error", err)
	}
}

func TestNilHandlerServesDefaultServeMux(t *testing.T) {
	t.Parallel()
	// A fresh path per run: -count=N reruns the test against the same global mux.
	path := "/atlas-default-mux-" + rand.Text()
	http.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "default mux")
	})
	svc := start(t, nil)

	if got := get(t, svc.url+path); got.status != http.StatusOK || got.body != "default mux" {
		t.Errorf("GET %s = %d %q, want 200 from http.DefaultServeMux", path, got.status, got.body)
	}
	if n := len(svc.logs.records(t, "panic recovered")); n != 0 {
		t.Errorf("got %d panic logs, want 0:\n%s", n, svc.logs)
	}
}

func TestRunRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	ok := func(context.Context) error { return nil }
	tests := []struct {
		name    string
		opt     atlas.Option
		wantErr string
	}{
		{"nil check", atlas.WithReadinessCheck("db", nil), `readiness check "db" is nil`},
		{"unnamed check", atlas.WithReadinessCheck("", ok), "readiness check with an empty name"},
		{"nil hook", atlas.WithShutdownHook("db", nil), `shutdown hook "db" is nil`},
		{"unnamed hook", atlas.WithShutdownHook("", ok), "shutdown hook with an empty name"},
		{"nil worker", atlas.WithBackground("consumer", nil), `background worker "consumer" is nil`},
		{"unnamed worker", atlas.WithBackground("", ok), "background worker with an empty name"},
		{"nil ops handler", atlas.WithOpsHandler("/debug/vars", nil), `ops handler "/debug/vars" is nil`},
		{"ops pattern with method", atlas.WithOpsHandler("GET /debug/vars", http.NotFoundHandler()), "must be a path"},
		{"ops pattern with wildcard", atlas.WithOpsHandler("/debug/{name}", http.NotFoundHandler()), "wildcards"},
		{"unclean ops pattern", atlas.WithOpsHandler("/debug/../vars", http.NotFoundHandler()), "not clean"},
		{"root ops pattern", atlas.WithOpsHandler("/", http.NotFoundHandler()), "every path"},
		{"reserved ops pattern", atlas.WithOpsHandler("/metrics", http.NotFoundHandler()), "reserved"},
		{"nil gatherer", atlas.WithPrometheusGatherer(nil), "prometheus gatherer is nil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertRejected(t, tt.wantErr, tt.opt)
		})
	}
	t.Run("duplicate check", func(t *testing.T) {
		t.Parallel()
		assertRejected(t, `readiness check "db" registered twice`,
			atlas.WithReadinessCheck("db", ok), atlas.WithReadinessCheck("db", ok))
	})
	t.Run("duplicate ops handler", func(t *testing.T) {
		t.Parallel()
		assertRejected(t, `ops handler "/debug/vars" registered twice`,
			atlas.WithOpsHandler("/debug/vars", http.NotFoundHandler()),
			atlas.WithOpsHandler("/debug/vars", http.NotFoundHandler()))
	})
}

// assertRejected checks that Run fails with wantErr before serving and closes the listener it was given.
func assertRejected(t *testing.T, wantErr string, opts ...atlas.Option) {
	t.Helper()
	ln := listenLocal(t)
	base := []atlas.Option{
		atlas.WithListener(ln),
		atlas.WithLogger(slog.New(slog.DiscardHandler)),
		atlas.WithoutGlobalTelemetry(),
	}
	err := atlas.Run(t.Context(), helloMux(), append(base, opts...)...)
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Errorf("Run() = %v, want an error containing %q", err, wantErr)
	}
	if err := ln.Close(); err == nil {
		t.Error("Run left the listener open")
	}
}

func TestShutdownHookPanicIsRecovered(t *testing.T) {
	t.Parallel()
	var ran []string
	svc := start(t, helloMux(),
		atlas.WithShutdownHook("db", func(context.Context) error {
			ran = append(ran, "db")
			return nil
		}),
		atlas.WithShutdownHook("cache", func(context.Context) error { panic("cache gone") }),
	)

	err := svc.stop()
	if err == nil || !strings.Contains(err.Error(), `shutdown hook "cache": panic: cache gone`) {
		t.Errorf("Run() = %v, want the hook panic as an error", err)
	}
	if !slices.Equal(ran, []string{"db"}) {
		t.Errorf("hooks run after the panic = %v, want [db]", ran)
	}
	panics := svc.logs.records(t, "panic recovered")
	if len(panics) != 1 || panics[0]["shutdown_hook"] != "cache" || !strings.Contains(fmt.Sprint(panics[0]["stack"]), "TestShutdownHookPanicIsRecovered") {
		t.Errorf("panic logs = %v, want one for hook cache with its stack", panics)
	}
	if len(svc.logs.records(t, "stopped")) != 1 {
		t.Errorf("missing stopped log:\n%s", svc.logs)
	}
}

func TestRegisterOnShutdownStopsLongLivedHandler(t *testing.T) {
	t.Parallel()
	closing := make(chan struct{})
	notify := sync.OnceFunc(func() { close(closing) })
	streaming := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "event: hello\n\n")
		_ = http.NewResponseController(w).Flush()
		close(streaming)
		select {
		case <-closing:
		case <-r.Context().Done():
		}
	})
	svc := start(t, handler,
		atlas.WithShutdownTimeout(waitTimeout),
		atlas.WithServerConfig(func(s *http.Server) { s.RegisterOnShutdown(notify) }),
	)

	resp, err := http.Get(svc.url + "/events")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	<-streaming

	start := time.Now()
	if err := svc.stop(); err != nil {
		t.Errorf("Run() = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > waitTimeout/2 {
		t.Errorf("shutdown took %v; the stream should end on RegisterOnShutdown", elapsed)
	}
}

func TestRecorderKeepsReadFromForServeFile(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("atlas ", 1000)
	file := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(io.ReaderFrom); !ok {
			http.Error(w, "ResponseWriter lost io.ReaderFrom", http.StatusInternalServerError)
			return
		}
		http.ServeFile(w, r, file)
	})
	svc := start(t, handler)

	if got := get(t, svc.url+"/data.txt"); got.status != http.StatusOK || got.body != content {
		t.Fatalf("GET /data.txt = %d with %d bytes, want 200 with the file (%s)", got.status, len(got.body), got.body[:min(len(got.body), 40)])
	}
	if err := svc.stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	access := svc.logs.records(t, "request")
	if len(access) != 1 || access[0]["status"] != 200.0 || access[0]["bytes"] != float64(len(content)) {
		t.Errorf("access log = %v, want status 200 and %d bytes", access, len(content))
	}
}
