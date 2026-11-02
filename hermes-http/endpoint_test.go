package hermes_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/svallejo-dev/olympus-core/hermes-http"
)

func endpoint(t *testing.T, c *hermes.Client, name string, opts ...hermes.Option) *hermes.Client {
	t.Helper()
	e, err := c.Endpoint(name, opts...)
	if err != nil {
		t.Fatalf("Endpoint(%q): %v", name, err)
	}
	return e
}

func TestEndpointRejectsInvalidConfig(t *testing.T) {
	c := newClient(t, "http://api.test")
	if _, err := c.Endpoint(""); err == nil {
		t.Error("Endpoint with an empty name: err = nil")
	}
	if _, err := c.Endpoint("x", hermes.WithTimeout(-time.Second)); err == nil {
		t.Error("Endpoint with an invalid option: err = nil")
	}
	if _, err := c.Endpoint("x"); err != nil {
		t.Errorf("Endpoint after a failed one: %v, a failure must not stick to c", err)
	}
}

func TestEndpointRetryIsItsOwnAndLeavesTheParentAlone(t *testing.T) {
	h, hits := statusSequence(503)
	srv := newServer(t, h)
	c := newClient(t, srv.URL, hermes.WithoutCircuitBreaker())
	once := endpoint(t, c, "once", hermes.WithoutRetry())
	five := endpoint(t, c, "five", hermes.WithRetry(hermes.RetryPolicy{
		MaxAttempts: 5, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond,
	}))

	for _, tt := range []struct {
		name string
		c    *hermes.Client
		want int32
	}{{"once", once, 1}, {"five", five, 5}, {"parent", c, 3}, {"inherited", endpoint(t, c, "inherited"), 3}} {
		hits.Store(0)
		mustGet(t, tt.c, "/")
		if got := hits.Load(); got != tt.want {
			t.Errorf("%s: requests = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestEndpointInheritsBaseAndHeadersWithoutChangingTheParent(t *testing.T) {
	var path, region, tier atomic.Value
	srv := newServer(t, func(_ http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		region.Store(r.Header.Get("X-Region"))
		tier.Store(r.Header.Get("X-Tier"))
	})
	c := newClient(t, srv.URL+"/v1", hermes.WithHeader("X-Region", "eu"))
	e := endpoint(t, c, "e", hermes.WithHeader("X-Tier", "gold"))

	mustGet(t, e, "/orders")
	if path.Load() != "/v1/orders" || region.Load() != "eu" || tier.Load() != "gold" {
		t.Errorf("endpoint sent path=%v region=%v tier=%v", path.Load(), region.Load(), tier.Load())
	}
	mustGet(t, c, "/orders")
	if tier.Load() != "" {
		t.Errorf("parent sent X-Tier=%v, the endpoint's header must stay on the endpoint", tier.Load())
	}
}

func TestEndpointTimeoutsAndResponseLimit(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(100 * time.Millisecond)
		}
		_, _ = fmt.Fprint(w, `{"name":"long enough"}`)
	})
	c := newClient(t, srv.URL, hermes.WithoutRetry())

	total := endpoint(t, c, "total", hermes.WithTimeout(10*time.Millisecond))
	if _, err := get(t, total, "/slow"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WithTimeout: err = %v, want DeadlineExceeded", err)
	}
	attempt := endpoint(t, c, "attempt", hermes.WithAttemptTimeout(10*time.Millisecond))
	if _, err := get(t, attempt, "/slow"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WithAttemptTimeout: err = %v, want DeadlineExceeded", err)
	}
	if code := mustGet(t, c, "/slow"); code != http.StatusOK {
		t.Errorf("parent: status = %d, want 200", code)
	}

	small := endpoint(t, c, "small", hermes.WithMaxResponseBytes(4))
	if _, err := hermes.Get[map[string]string](t.Context(), small, "/"); err == nil || !strings.Contains(err.Error(), "exceeds 4 bytes") {
		t.Errorf("WithMaxResponseBytes: err = %v, want the size limit", err)
	}
	if _, err := hermes.Get[map[string]string](t.Context(), c, "/"); err != nil {
		t.Errorf("parent: %v", err)
	}
}

func TestEndpointBreaker(t *testing.T) {
	h, _ := statusSequence(500)
	srv := newServer(t, h)
	tripAfterOne := hermes.WithCircuitBreaker(hermes.BreakerPolicy{ConsecutiveFailures: 1})

	t.Run("left out, it shares the parent's", func(t *testing.T) {
		c := newClient(t, srv.URL, hermes.WithoutRetry(), tripAfterOne)
		e := endpoint(t, c, "shared", hermes.WithAttemptTimeout(time.Second))
		mustGet(t, e, "/")
		if _, err := get(t, c, "/"); !errors.Is(err, hermes.ErrCircuitOpen) {
			t.Errorf("parent: err = %v, want ErrCircuitOpen", err)
		}
	})
	t.Run("passed, a failing endpoint does not open the parent's", func(t *testing.T) {
		c := newClient(t, srv.URL, hermes.WithoutRetry(), tripAfterOne)
		e := endpoint(t, c, "own", tripAfterOne)
		mustGet(t, e, "/")
		_, err := get(t, e, "/")
		if !errors.Is(err, hermes.ErrCircuitOpen) {
			t.Fatalf("endpoint: err = %v, want ErrCircuitOpen", err)
		}
		if !strings.Contains(err.Error(), "own http://") {
			t.Errorf("endpoint: err = %q, want the endpoint's name", err)
		}
		if code := mustGet(t, c, "/"); code != http.StatusInternalServerError {
			t.Errorf("parent: status = %d, want 500: its breaker must still be closed", code)
		}
	})
	t.Run("removed, it never rejects", func(t *testing.T) {
		c := newClient(t, srv.URL, hermes.WithoutRetry(), tripAfterOne)
		e := endpoint(t, c, "none", hermes.WithoutCircuitBreaker())
		for range 3 {
			if code := mustGet(t, e, "/"); code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", code)
			}
		}
	})
}

// A limiter of one token an hour lets exactly one attempt through, so the second caller either waits or does not.
func TestEndpointRateLimit(t *testing.T) {
	h, _ := statusSequence(200)
	srv := newServer(t, h)
	spent := func(t *testing.T) *hermes.Client {
		t.Helper()
		c := newClient(t, srv.URL, hermes.WithRateLimit(1.0/3600, 1))
		mustGet(t, c, "/")
		return c
	}

	if _, err := get(t, endpoint(t, spent(t), "shared"), "/"); err == nil {
		t.Error("left out: err = nil, the endpoint must draw on the parent's tokens")
	}
	if code := mustGet(t, endpoint(t, spent(t), "own", hermes.WithRateLimit(1000, 1)), "/"); code != http.StatusOK {
		t.Errorf("passed: status = %d, want 200", code)
	}
	if code := mustGet(t, endpoint(t, spent(t), "none", hermes.WithoutRateLimit()), "/"); code != http.StatusOK {
		t.Errorf("removed: status = %d, want 200", code)
	}
}

func TestEndpointMaxConcurrent(t *testing.T) {
	h, _ := statusSequence(200)
	srv := newServer(t, h)
	busy := func(t *testing.T) *hermes.Client {
		t.Helper()
		c := newClient(t, srv.URL, hermes.WithMaxConcurrent(1))
		resp, err := get(t, c, "/")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return c
	}

	shared := endpoint(t, busy(t), "shared")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	req, err := shared.NewRequest(ctx, http.MethodGet, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shared.Do(req); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("left out: err = %v, want DeadlineExceeded waiting for the parent's slot", err)
	}
	if code := mustGet(t, endpoint(t, busy(t), "own", hermes.WithMaxConcurrent(1)), "/"); code != http.StatusOK {
		t.Errorf("passed: status = %d, want 200", code)
	}
	if code := mustGet(t, endpoint(t, busy(t), "none", hermes.WithoutMaxConcurrent()), "/"); code != http.StatusOK {
		t.Errorf("removed: status = %d, want 200", code)
	}
}

func TestEndpointHTTPClientUsesItsPipeline(t *testing.T) {
	h, hits := statusSequence(503)
	srv := newServer(t, h)
	e := endpoint(t, newClient(t, srv.URL, hermes.WithoutCircuitBreaker()), "once", hermes.WithoutRetry())

	resp, err := e.HTTPClient().Get(srv.URL) //nolint:noctx // the test only counts requests
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if hits.Load() != 1 {
		t.Errorf("requests = %d, want 1", hits.Load())
	}
}

func TestEndpointNameLabelsTelemetry(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	h, _ := statusSequence(500)
	srv := newServer(t, h)
	tripAfterOne := hermes.WithCircuitBreaker(hermes.BreakerPolicy{ConsecutiveFailures: 1})
	c := newClient(t, srv.URL, hermes.WithMeterProvider(mp), hermes.WithTracerProvider(tp), hermes.WithoutRetry(), tripAfterOne)
	e := endpoint(t, c, "payments", tripAfterOne, hermes.WithRetry(hermes.RetryPolicy{
		MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond,
		ShouldRetry: func(*http.Response, error) bool { return true },
	}))

	mustGet(t, c, "/")
	if _, err := get(t, c, "/"); !errors.Is(err, hermes.ErrCircuitOpen) {
		t.Fatalf("parent: err = %v, want ErrCircuitOpen", err)
	}
	// The first attempt opens the endpoint's breaker, so the retry is rejected.
	if _, err := get(t, e, "/"); !errors.Is(err, hermes.ErrCircuitOpen) {
		t.Fatalf("endpoint: err = %v, want ErrCircuitOpen", err)
	}
	metrics := collectMetrics(t, reader)

	retries := sumPoints(t, metrics["hermes.client.retries"])
	if len(retries) != 1 || attr(retries[0].Attributes, "hermes.endpoint") != "payments" {
		t.Errorf("hermes.client.retries = %+v, want one point for payments", retries)
	}
	rejections := map[string]int64{}
	for _, p := range sumPoints(t, metrics["hermes.client.circuit_breaker.rejections"]) {
		rejections[attr(p.Attributes, "hermes.endpoint")] = p.Value
	}
	if len(rejections) != 2 || rejections[""] != 1 || rejections["payments"] != 1 {
		t.Errorf("rejections by endpoint = %v, want one for the parent and one for payments", rejections)
	}
	state, ok := metrics["hermes.client.circuit_breaker.state"].(metricdata.Gauge[int64])
	if !ok || len(state.DataPoints) != 2 {
		t.Fatalf("breaker state = %+v, want one series for the parent and one for payments", metrics["hermes.client.circuit_breaker.state"])
	}
	duration, ok := metrics["http.client.request.duration"].(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("http.client.request.duration = %T", metrics["http.client.request.duration"])
	}
	seen := map[string]bool{}
	for _, p := range duration.DataPoints {
		seen[attr(p.Attributes, "hermes.endpoint")] = true
	}
	if !seen["payments"] || !seen[""] {
		t.Errorf("http.client.request.duration endpoints = %v, want payments and the unlabelled parent", seen)
	}

	var labelled int
	for _, s := range recorder.Ended() {
		for _, a := range s.Attributes() {
			if string(a.Key) == "hermes.endpoint" && a.Value.AsString() == "payments" {
				labelled++
			}
		}
	}
	if labelled != 1 {
		t.Errorf("spans labelled payments = %d, want 1", labelled)
	}
}

func TestBearerTokenCallIsNotLabelledAsTheEndpoint(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	h, _ := statusSequence(200)
	srv := newServer(t, h)

	auth := newClient(t, srv.URL, hermes.WithMeterProvider(mp))
	authNamed := endpoint(t, auth, "auth")
	for _, tokenClient := range []*hermes.Client{auth, authNamed} {
		c := newClient(t, srv.URL, hermes.WithMeterProvider(mp), hermes.WithBearerToken(func(ctx context.Context) (string, error) {
			req, err := tokenClient.NewRequest(ctx, http.MethodGet, "/token", nil)
			if err != nil {
				return "", err
			}
			resp, err := tokenClient.Do(req)
			if err != nil {
				return "", err
			}
			return "t", resp.Body.Close()
		}))
		mustGet(t, endpoint(t, c, "payments"), "/")
	}

	duration, ok := collectMetrics(t, reader)["http.client.request.duration"].(metricdata.Histogram[float64])
	if !ok {
		t.Fatal("http.client.request.duration not recorded")
	}
	counts := map[string]uint64{}
	for _, p := range duration.DataPoints {
		counts[attr(p.Attributes, "hermes.endpoint")] += p.Count
	}
	if counts["payments"] != 2 || counts["auth"] != 1 || counts[""] != 1 {
		t.Errorf("calls by endpoint = %v, want payments:2 auth:1 and one unlabelled", counts)
	}
}

func TestWithRetryKeepsTheFieldsItLeavesAtZero(t *testing.T) {
	h, hits := statusSequence(503)
	srv := newServer(t, h)
	post := func(t *testing.T, c *hermes.Client, opts ...hermes.RequestOption) int32 {
		t.Helper()
		hits.Store(0)
		_, _ = hermes.Post[struct{}](t.Context(), c, "/", struct{}{}, opts...)
		return hits.Load()
	}
	c := newClient(t, srv.URL, hermes.WithoutCircuitBreaker(), hermes.WithRetry(hermes.RetryPolicy{
		MaxAttempts:       4,
		IdempotencyHeader: "X-Request-Id",
	}))

	t.Run("an inherited field applies", func(t *testing.T) {
		e := endpoint(t, c, "e", hermes.WithRetry(hermes.RetryPolicy{MaxBackoff: 2 * time.Millisecond}))
		if got := post(t, e, hermes.Header("X-Request-Id", "k")); got != 4 {
			t.Errorf("requests = %d, want the parent's MaxAttempts and IdempotencyHeader to give 4", got)
		}
	})
	t.Run("a field it writes wins", func(t *testing.T) {
		e := endpoint(t, c, "e", hermes.WithRetry(hermes.RetryPolicy{MaxAttempts: 2}))
		if got := post(t, e, hermes.Header("X-Request-Id", "k")); got != 2 {
			t.Errorf("requests = %d, want 2", got)
		}
	})
	t.Run("the parent keeps its own", func(t *testing.T) {
		if got := post(t, c, hermes.Header("X-Request-Id", "k")); got != 4 {
			t.Errorf("requests = %d, want 4", got)
		}
	})
	t.Run("WithoutRetry first starts from the defaults", func(t *testing.T) {
		e := endpoint(t, c, "e", hermes.WithoutRetry(), hermes.WithRetry(hermes.RetryPolicy{InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}))
		if got := post(t, e, hermes.Header("X-Request-Id", "k")); got != 1 {
			t.Errorf("requests with the parent's header = %d, want 1: it must be forgotten", got)
		}
		if got := post(t, e, hermes.IdempotencyKey("k")); got != 3 {
			t.Errorf("requests with Idempotency-Key = %d, want the default 3", got)
		}
	})
	t.Run("options in New add up too", func(t *testing.T) {
		n := newClient(t, srv.URL, hermes.WithoutCircuitBreaker(),
			hermes.WithRetry(hermes.RetryPolicy{MaxAttempts: 2}),
			hermes.WithRetry(hermes.RetryPolicy{RetryNonIdempotent: true}),
		)
		if got := post(t, n); got != 2 {
			t.Errorf("requests = %d, want 2", got)
		}
	})
}

func TestWithCircuitBreakerKeepsTheFieldsItLeavesAtZero(t *testing.T) {
	h, _ := statusSequence(500)
	srv := newServer(t, h)
	c := newClient(t, srv.URL, hermes.WithoutRetry(), hermes.WithCircuitBreaker(hermes.BreakerPolicy{ConsecutiveFailures: 1}))

	inherited := endpoint(t, c, "inherited", hermes.WithCircuitBreaker(hermes.BreakerPolicy{OpenTimeout: time.Hour}))
	mustGet(t, inherited, "/")
	if _, err := get(t, inherited, "/"); !errors.Is(err, hermes.ErrCircuitOpen) {
		t.Errorf("err = %v, want the parent's ConsecutiveFailures to open it after one failure", err)
	}
	if code := mustGet(t, c, "/"); code != http.StatusInternalServerError {
		t.Errorf("parent: status = %d, want 500: the endpoint's state must stay its own", code)
	}

	fresh := endpoint(t, c, "fresh", hermes.WithoutCircuitBreaker(), hermes.WithCircuitBreaker(hermes.BreakerPolicy{OpenTimeout: time.Hour}))
	for range 2 {
		if code := mustGet(t, fresh, "/"); code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500: the default of 5 failures must apply", code)
		}
	}
}
