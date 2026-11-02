package atlas

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

type telemetry struct {
	serviceName    string
	gatherer       mergedGatherer
	meterProvider  metric.MeterProvider
	tracerProvider trace.TracerProvider
	propagator     propagation.TextMapPropagator
	shutdowns      []func(context.Context) error
}

func newTelemetry(ctx context.Context, cfg *config) (*telemetry, error) {
	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithAttributes(semconv.ServiceName(cfg.serviceName)),
		resource.WithFromEnv(),
	)
	if err != nil {
		if !errors.Is(err, resource.ErrPartialResource) {
			return nil, fmt.Errorf("build telemetry resource: %w", err)
		}
		cfg.logger.WarnContext(ctx, "ignoring invalid OpenTelemetry resource attributes", "err", err)
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	serviceName, _ := res.Set().Value(semconv.ServiceNameKey)
	t := &telemetry{
		serviceName:    serviceName.AsString(),
		gatherer:       append(mergedGatherer{registry}, cfg.gatherers...),
		meterProvider:  cfg.meterProvider,
		tracerProvider: cfg.tracerProvider,
		propagator:     propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
	}

	if t.meterProvider == nil {
		exporter, err := otelprom.New(otelprom.WithRegisterer(registry))
		if err != nil {
			return nil, fmt.Errorf("create prometheus exporter: %w", err)
		}
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter), sdkmetric.WithResource(res))
		t.meterProvider = mp
		t.shutdowns = append(t.shutdowns, mp.Shutdown)
	}

	if t.tracerProvider == nil && otlpConfigured() {
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("create otlp trace exporter: %w", err), t.shutdown(ctx))
		}
		tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
		t.tracerProvider = tp
		t.shutdowns = append(t.shutdowns, tp.Shutdown)
	}

	if cfg.globalTelemetry {
		t.setGlobals(cfg.logger)
	}
	if t.tracerProvider == nil {
		t.tracerProvider = otel.GetTracerProvider()
	}
	return t, nil
}

func otlpConfigured() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

func (t *telemetry) setGlobals(logger *slog.Logger) {
	otel.SetMeterProvider(t.meterProvider)
	if t.tracerProvider != nil {
		otel.SetTracerProvider(t.tracerProvider)
	}
	otel.SetTextMapPropagator(t.propagator)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Error("opentelemetry", "err", err)
	}))
	// SetDefault points package log at the new handler, so one writing through package log would deadlock.
	if h, ok := logger.Handler().(contextHandler); !ok || !h.writesThroughLog() {
		slog.SetDefault(logger)
	}
}

func (t *telemetry) instrument(next http.Handler) http.Handler {
	return otelhttp.NewHandler(next, "http.server",
		otelhttp.WithMeterProvider(t.meterProvider),
		otelhttp.WithTracerProvider(t.tracerProvider),
		otelhttp.WithPropagators(t.propagator),
	)
}

func (t *telemetry) metricsHandler() http.Handler {
	return promhttp.HandlerFor(t.gatherer, promhttp.HandlerOpts{})
}

// mergedGatherer keeps the first family of each name instead of failing like prometheus.Gatherers:
// prometheus.DefaultGatherer has its own Go and process collectors, which atlas' registry also has.
type mergedGatherer []prometheus.Gatherer

func (m mergedGatherer) Gather() ([]*dto.MetricFamily, error) {
	var (
		families []*dto.MetricFamily
		errs     []error
	)
	seen := make(map[string]bool)
	for _, g := range m {
		mfs, err := g.Gather()
		errs = append(errs, err)
		for _, mf := range mfs {
			if !seen[mf.GetName()] {
				seen[mf.GetName()] = true
				families = append(families, mf)
			}
		}
	}
	slices.SortFunc(families, func(a, b *dto.MetricFamily) int { return strings.Compare(a.GetName(), b.GetName()) })
	return families, errors.Join(errs...)
}

// shutdown flushes only the providers newTelemetry created; user-supplied ones belong to the caller.
func (t *telemetry) shutdown(ctx context.Context) error {
	var errs []error
	for _, shutdown := range t.shutdowns {
		if err := shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown telemetry: %w", err))
		}
	}
	return errors.Join(errs...)
}
