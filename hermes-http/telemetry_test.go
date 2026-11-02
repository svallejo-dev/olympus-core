package hermes_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

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
