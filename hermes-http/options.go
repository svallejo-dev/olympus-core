package hermes

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/time/rate"
)

const (
	defaultTimeout          = 30 * time.Second
	defaultAttemptTimeout   = 10 * time.Second
	defaultMaxResponseBytes = 10 << 20
)

// Option configures a Client in New; options apply in order, so a later one wins.
type Option func(*config)

type config struct {
	name             string
	timeout          time.Duration
	attemptTimeout   time.Duration
	retry            *RetryPolicy
	breaker          *BreakerPolicy
	breakers         *breakerGroup
	slots            chan struct{}
	limiter          *rate.Limiter
	headers          http.Header
	bearerToken      func(context.Context) (string, error)
	transport        http.RoundTripper
	maxResponseBytes int64
	logger           *slog.Logger
	meterProvider    metric.MeterProvider
	tracerProvider   trace.TracerProvider
	propagators      propagation.TextMapPropagator
	errs             []error
}

func newConfig(opts []Option) config {
	retry, breaker := RetryPolicy{}.withDefaults(), BreakerPolicy{}.withDefaults()
	cfg := config{
		timeout:          defaultTimeout,
		attemptTimeout:   defaultAttemptTimeout,
		retry:            &retry,
		breaker:          &breaker,
		headers:          make(http.Header),
		maxResponseBytes: defaultMaxResponseBytes,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

func (c *config) fail(format string, args ...any) {
	c.errs = append(c.errs, fmt.Errorf("hermes: "+format, args...))
}

// WithTimeout bounds a whole call, retries, redirects and body read included, as http.Client.Timeout (default 30s; 0 disables it).
func WithTimeout(d time.Duration) Option {
	return func(c *config) {
		if d < 0 {
			c.fail("WithTimeout(%v): negative duration", d)
			return
		}
		c.timeout = d
	}
}

// WithAttemptTimeout bounds each attempt until its response headers arrive (default 10s; 0 disables it).
func WithAttemptTimeout(d time.Duration) Option {
	return func(c *config) {
		if d < 0 {
			c.fail("WithAttemptTimeout(%v): negative duration", d)
			return
		}
		c.attemptTimeout = d
	}
}

// WithRetry changes the non-zero fields of p and keeps the rest: on an Endpoint, the values of its parent; otherwise the defaults documented on RetryPolicy.
func WithRetry(p RetryPolicy) Option {
	return func(c *config) {
		if err := p.validate(); err != nil {
			c.errs = append(c.errs, err)
			return
		}
		if c.retry != nil {
			p = p.over(*c.retry)
		}
		p = p.withDefaults()
		c.retry = &p
	}
}

// WithoutRetry sends every request exactly once; a WithRetry after it starts again from the defaults, which also clears inherited fields.
func WithoutRetry() Option {
	return func(c *config) { c.retry = nil }
}

// WithCircuitBreaker changes the non-zero fields of p and keeps the rest, as WithRetry does; on an Endpoint it also gives it breakers of its own.
func WithCircuitBreaker(p BreakerPolicy) Option {
	return func(c *config) {
		if err := p.validate(); err != nil {
			c.errs = append(c.errs, err)
			return
		}
		if c.breaker != nil {
			p = p.over(*c.breaker)
		}
		p = p.withDefaults()
		c.breaker, c.breakers = &p, nil
	}
}

// WithoutCircuitBreaker disables the per-host circuit breakers; a WithCircuitBreaker after it starts again from the defaults.
func WithoutCircuitBreaker() Option {
	return func(c *config) { c.breaker, c.breakers = nil, nil }
}

// WithMaxConcurrent caps the calls in flight at n (a bulkhead), across the Client and the endpoints that share its cap; a call holds its slot until the response body is closed, and waiting honours the context.
func WithMaxConcurrent(n int) Option {
	return func(c *config) {
		if n < 1 {
			c.fail("WithMaxConcurrent(%d): must be at least 1", n)
			return
		}
		c.slots = make(chan struct{}, n)
	}
}

// WithoutMaxConcurrent removes the cap on calls in flight, for an Endpoint that must not wait behind the others.
func WithoutMaxConcurrent() Option {
	return func(c *config) { c.slots = nil }
}

// WithRateLimit paces attempts, retries included, to perSecond with bursts of up to burst, across the Client and the endpoints that share its limit; a wait that would outlast the deadline fails at once with an error wrapping context.DeadlineExceeded.
func WithRateLimit(perSecond float64, burst int) Option {
	return func(c *config) {
		if perSecond <= 0 || math.IsNaN(perSecond) || math.IsInf(perSecond, 0) {
			c.fail("WithRateLimit(%v, %d): rate must be a positive finite number", perSecond, burst)
			return
		}
		if burst < 1 {
			c.fail("WithRateLimit(%v, %d): burst must be at least 1", perSecond, burst)
			return
		}
		c.limiter = rate.NewLimiter(rate.Limit(perSecond), burst)
	}
}

// WithoutRateLimit removes the rate limit, for an Endpoint that must not wait behind the others.
func WithoutRateLimit() Option {
	return func(c *config) { c.limiter = nil }
}

// WithHeader sets a header on every request unless the request already carries that header.
func WithHeader(key, value string) Option {
	return func(c *config) { c.headers.Set(key, value) }
}

// WithUserAgent sets the User-Agent of every request that does not set its own.
func WithUserAgent(ua string) Option {
	return WithHeader("User-Agent", ua)
}

// WithBearerToken sends "Authorization: Bearer <token>" with a token obtained on every attempt, so refreshed tokens are picked up by retries.
func WithBearerToken(token func(ctx context.Context) (string, error)) Option {
	return func(c *config) {
		if token == nil {
			c.fail("WithBearerToken: nil token function")
			return
		}
		c.bearerToken = token
	}
}

// WithTransport sets the base transport under the pipeline (default: a clone of http.DefaultTransport tuned for API calls).
func WithTransport(rt http.RoundTripper) Option {
	return func(c *config) {
		if rt == nil {
			c.fail("WithTransport: nil transport")
			return
		}
		c.transport = rt
	}
}

// WithMaxResponseBytes caps the response body the JSON helpers decode (default 10 MiB).
func WithMaxResponseBytes(n int64) Option {
	return func(c *config) {
		if n < 1 {
			c.fail("WithMaxResponseBytes(%d): must be at least 1", n)
			return
		}
		c.maxResponseBytes = n
	}
}

// WithLogger sets the logger for breaker state changes (Warn) and retries (Debug); nil means slog.Default at log time.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) { c.logger = l }
}

// WithMeterProvider sets the meter provider for hermes and otelhttp metrics (default: the OTel global).
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) { c.meterProvider = mp }
}

// WithTracerProvider sets the tracer provider for client spans (default: the OTel global).
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) { c.tracerProvider = tp }
}

// WithPropagators sets the propagator that injects trace context into requests (default: the OTel global).
func WithPropagators(p propagation.TextMapPropagator) Option {
	return func(c *config) { c.propagators = p }
}
