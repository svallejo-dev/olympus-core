package hermes_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/svallejo-dev/olympus-core/hermes-http"
)

func TestMetricsAreRecorded(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	flaky, _ := statusSequence(503, 200)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/down" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		flaky(w, r)
	})
	c := newClient(t, srv.URL,
		hermes.WithMeterProvider(mp),
		hermes.WithCircuitBreaker(hermes.BreakerPolicy{ConsecutiveFailures: 2}),
	)

	mustGet(t, c, "/flaky")
	mustGet(t, c, "/down")
	mustGet(t, c, "/down")
	if _, err := get(t, c, "/down"); !errors.Is(err, hermes.ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}

	metrics := collectMetrics(t, reader)

	retries := sumPoints(t, metrics["hermes.client.retries"])
	if len(retries) != 1 || retries[0].Value != 1 || attr(retries[0].Attributes, "hermes.retry.reason") != "503" ||
		attr(retries[0].Attributes, "server.address") != "127.0.0.1" {
		t.Errorf("hermes.client.retries = %+v, want one retry with reason 503 for 127.0.0.1", retries)
	}
	rejections := sumPoints(t, metrics["hermes.client.circuit_breaker.rejections"])
	if len(rejections) != 1 || rejections[0].Value != 1 {
		t.Errorf("hermes.client.circuit_breaker.rejections = %+v, want 1", rejections)
	}
	state, ok := metrics["hermes.client.circuit_breaker.state"].(metricdata.Gauge[int64])
	if !ok || len(state.DataPoints) != 1 || state.DataPoints[0].Value != 2 {
		t.Errorf("hermes.client.circuit_breaker.state = %+v, want 2 (open)", metrics["hermes.client.circuit_breaker.state"])
	}
	if _, ok := metrics["http.client.request.duration"]; !ok {
		t.Error("otelhttp metric http.client.request.duration not recorded")
	}
}

func collectMetrics(t *testing.T, reader sdkmetric.Reader) map[string]metricdata.Aggregation {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	metrics := make(map[string]metricdata.Aggregation)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			metrics[m.Name] = m.Data
		}
	}
	return metrics
}

func sumPoints(t *testing.T, agg metricdata.Aggregation) []metricdata.DataPoint[int64] {
	t.Helper()
	sum, ok := agg.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("aggregation = %T, want metricdata.Sum[int64]", agg)
	}
	return sum.DataPoints
}

func attr(set attribute.Set, key string) string {
	v, _ := set.Value(attribute.Key(key))
	return v.String()
}

func TestTraceContextIsInjectedOncePerCall(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	var (
		mu      sync.Mutex
		parents []string
	)
	codes, _ := statusSequence(503, 200)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		parents = append(parents, r.Header.Get("Traceparent"))
		mu.Unlock()
		codes(w, r)
	})
	c := newClient(t, srv.URL, hermes.WithTracerProvider(tp), hermes.WithPropagators(propagation.TraceContext{}))

	mustGet(t, c, "/")

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1 client span for the logical call", len(spans))
	}
	sc := spans[0].SpanContext()
	want := "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-01"
	if len(parents) != 2 || parents[0] != want || parents[1] != want {
		t.Errorf("traceparent headers = %q, want %q on both attempts", parents, want)
	}
	var retryEvents int
	for _, e := range spans[0].Events() {
		if strings.HasPrefix(e.Name, "hermes.retry") {
			retryEvents++
		}
	}
	if retryEvents != 1 {
		t.Errorf("retry span events = %d, want 1", retryEvents)
	}
}

// errorTypes collects the error.type values recorded on the client duration metric.
func errorTypes(t *testing.T, reader sdkmetric.Reader) []string {
	t.Helper()
	hist, _ := collectMetrics(t, reader)["http.client.request.duration"].(metricdata.Histogram[float64])
	var got []string
	for _, dp := range hist.DataPoints {
		if v, ok := dp.Attributes.Value("error.type"); ok {
			got = append(got, v.AsString())
		}
	}
	return got
}

func TestErrorTypeNamesTheFailure(t *testing.T) {
	const pkg = "github.com/svallejo-dev/olympus-core/hermes-http."
	block := func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	fail := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }
	retryLater := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	shortly := func(t *testing.T) context.Context {
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		t.Cleanup(cancel)
		return ctx
	}
	fetch := func(ctx context.Context, c *hermes.Client) { _, _ = hermes.GetJSON[struct{}](ctx, c, "/") }

	tests := []struct {
		name    string
		handler http.HandlerFunc
		opts    []hermes.Option
		call    func(t *testing.T, c *hermes.Client)
		want    string
	}{
		{"open breaker", fail, []hermes.Option{hermes.WithoutRetry(), hermes.WithCircuitBreaker(hermes.BreakerPolicy{ConsecutiveFailures: 1})},
			func(t *testing.T, c *hermes.Client) {
				fetch(t.Context(), c)
				fetch(t.Context(), c)
			}, "circuitOpenError"},
		{"attempt timeout", block, []hermes.Option{hermes.WithoutRetry(), hermes.WithAttemptTimeout(20 * time.Millisecond)},
			func(t *testing.T, c *hermes.Client) { fetch(t.Context(), c) }, "attemptTimeoutError"},
		{"rate limit", fail, []hermes.Option{hermes.WithoutRetry(), hermes.WithRateLimit(0.001, 1)},
			func(t *testing.T, c *hermes.Client) {
				fetch(t.Context(), c)
				fetch(shortly(t), c)
			}, "rateLimitError"},
		{"concurrency slot", block, []hermes.Option{hermes.WithoutRetry(), hermes.WithMaxConcurrent(1)},
			func(t *testing.T, c *hermes.Client) {
				holder, release := context.WithCancel(t.Context())
				var wg sync.WaitGroup
				wg.Go(func() { fetch(holder, c) })
				time.Sleep(50 * time.Millisecond)
				fetch(shortly(t), c)
				release()
				wg.Wait()
			}, "concurrencyError"},
		{"retry wait", retryLater, nil,
			func(t *testing.T, c *hermes.Client) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				time.AfterFunc(100*time.Millisecond, cancel)
				fetch(ctx, c)
			}, "retryWaitError"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			c := newClient(t, newServer(t, tt.handler).URL, append(tt.opts, hermes.WithMeterProvider(mp))...)

			tt.call(t, c)

			if got := errorTypes(t, reader); !slices.Contains(got, pkg+tt.want) {
				t.Errorf("error.type values = %q, want %q among them", got, pkg+tt.want)
			}
		})
	}
}
