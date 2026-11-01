package atlas

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const (
	defaultPort            = "8080"
	defaultShutdownTimeout = 20 * time.Second
	kubernetesDrainDelay   = 5 * time.Second
)

// Option configures Run.
type Option func(*config)

type config struct {
	addr            string
	adminAddr       string
	listener        net.Listener
	adminListener   net.Listener
	logger          *slog.Logger
	serviceName     string
	checks          []*check
	hooks           []namedFunc
	workers         []namedFunc
	opsHandlers     []opsHandler
	gatherers       []prometheus.Gatherer
	shutdownDelay   time.Duration
	shutdownTimeout time.Duration
	serverConfig    func(*http.Server)
	meterProvider   metric.MeterProvider
	tracerProvider  trace.TracerProvider
	globalTelemetry bool
	accessLog       bool
}

type namedFunc struct {
	name string
	fn   func(ctx context.Context) error
}

type opsHandler struct {
	pattern string
	handler http.Handler
}

func newConfig(opts []Option) config {
	cfg := config{
		addr:            defaultAddr(),
		logger:          slog.New(slog.NewJSONHandler(os.Stdout, nil)),
		serviceName:     filepath.Base(os.Args[0]),
		shutdownDelay:   defaultShutdownDelay(),
		shutdownTimeout: defaultShutdownTimeout,
		globalTelemetry: true,
		accessLog:       true,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// validate reports every invalid option at once, so Run fails before it listens.
func (c *config) validate() error {
	var errs []error
	checkNames := make(map[string]bool, len(c.checks))
	for _, ch := range c.checks {
		if checkNames[ch.name] {
			errs = append(errs, fmt.Errorf("readiness check %q registered twice", ch.name))
		}
		checkNames[ch.name] = true
		errs = append(errs, validateNamed("readiness check", ch.name, ch.fn == nil))
	}
	for _, h := range c.hooks {
		errs = append(errs, validateNamed("shutdown hook", h.name, h.fn == nil))
	}
	for _, w := range c.workers {
		errs = append(errs, validateNamed("background worker", w.name, w.fn == nil))
	}
	patterns := make(map[string]bool, len(c.opsHandlers))
	for _, oh := range c.opsHandlers {
		if patterns[oh.pattern] {
			errs = append(errs, fmt.Errorf("ops handler %q registered twice", oh.pattern))
		}
		patterns[oh.pattern] = true
		errs = append(errs, validateOpsPattern(oh.pattern))
		if oh.handler == nil {
			errs = append(errs, fmt.Errorf("ops handler %q is nil", oh.pattern))
		}
	}
	for _, g := range c.gatherers {
		if g == nil {
			errs = append(errs, errors.New("prometheus gatherer is nil"))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	return nil
}

func validateNamed(kind, name string, isNil bool) error {
	if name == "" {
		return fmt.Errorf("%s with an empty name", kind)
	}
	if isNil {
		return fmt.Errorf("%s %q is nil", kind, name)
	}
	return nil
}

func defaultAddr() string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":" + defaultPort
}

func defaultShutdownDelay() time.Duration {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return kubernetesDrainDelay
	}
	return 0
}

// WithAddr sets the app server's listen address; it overrides $PORT and the ":8080" default.
func WithAddr(addr string) Option {
	return func(c *config) { c.addr = addr }
}

// WithAdminAddr serves the ops endpoints on a separate server at addr instead of the app server.
func WithAdminAddr(addr string) Option {
	return func(c *config) { c.adminAddr = addr }
}

// WithListener serves the app on ln instead of listening on the address; Run closes ln.
func WithListener(ln net.Listener) Option {
	return func(c *config) { c.listener = ln }
}

// WithAdminListener serves the ops endpoints on ln, on a separate server; Run closes ln.
func WithAdminListener(ln net.Listener) Option {
	return func(c *config) { c.adminListener = ln }
}

// WithLogger sets the logger for lifecycle, access and panic logs; nil keeps the JSON stdout default.
func WithLogger(logger *slog.Logger) Option {
	return func(c *config) {
		if logger != nil {
			c.logger = logger
		}
	}
}

// WithServiceName sets the OpenTelemetry service.name; OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override it.
func WithServiceName(name string) Option {
	return func(c *config) { c.serviceName = name }
}

// WithReadinessCheck adds a check run concurrently, with a 2s timeout, by /readyz and /healthz; names must be unique.
func WithReadinessCheck(name string, fn Check) Option {
	return func(c *config) {
		c.checks = append(c.checks, &check{name: name, fn: fn, timeout: checkTimeout})
	}
}

// WithShutdownHook adds a hook run after the servers and background workers stop, in reverse registration order.
func WithShutdownHook(name string, hook func(ctx context.Context) error) Option {
	return func(c *config) { c.hooks = append(c.hooks, namedFunc{name: name, fn: hook}) }
}

// WithBackground runs fn from when the listeners are bound until shutdown cancels its ctx; an early error or panic stops Run with it.
func WithBackground(name string, fn func(ctx context.Context) error) Option {
	return func(c *config) { c.workers = append(c.workers, namedFunc{name: name, fn: fn}) }
}

// WithOpsHandler serves h for any method on the ops server at pattern, a clean path matched exactly or, ending in "/", as a subtree.
func WithOpsHandler(pattern string, h http.Handler) Option {
	return func(c *config) { c.opsHandlers = append(c.opsHandlers, opsHandler{pattern: pattern, handler: h}) }
}

// WithPrometheusGatherer adds g's metrics to /metrics; on a metric name clash atlas' own registry wins.
func WithPrometheusGatherer(g prometheus.Gatherer) Option {
	return func(c *config) { c.gatherers = append(c.gatherers, g) }
}

// WithShutdownDelay sets how long /readyz reports 503 before the servers stop accepting connections.
func WithShutdownDelay(d time.Duration) Option {
	return func(c *config) { c.shutdownDelay = d }
}

// WithShutdownTimeout bounds, after the shutdown delay, the time for draining requests and workers, hooks and telemetry flush.
func WithShutdownTimeout(d time.Duration) Option {
	return func(c *config) { c.shutdownTimeout = d }
}

// WithServerConfig adjusts every http.Server after the defaults are applied, e.g. to RegisterOnShutdown; the last call wins.
func WithServerConfig(configure func(*http.Server)) Option {
	return func(c *config) { c.serverConfig = configure }
}

// WithMeterProvider replaces the built-in Prometheus-backed provider; /metrics then serves only runtime and gatherer metrics.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) { c.meterProvider = mp }
}

// WithTracerProvider replaces the tracer provider configured from the OTLP environment variables.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) { c.tracerProvider = tp }
}

// WithoutGlobalTelemetry stops Run from setting the global OpenTelemetry providers and propagator.
func WithoutGlobalTelemetry() Option {
	return func(c *config) { c.globalTelemetry = false }
}

// WithoutAccessLog disables the per-request log line.
func WithoutAccessLog() Option {
	return func(c *config) { c.accessLog = false }
}
