package hermes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ErrCircuitOpen is returned, wrapped, when a host's circuit breaker rejects a request without sending it.
var ErrCircuitOpen = errors.New("hermes: circuit breaker open")

// BreakerPolicy configures the in-memory circuit breaker kept per scheme, host and port; zero fields take the defaults.
type BreakerPolicy struct {
	// ConsecutiveFailures opens the breaker after that many failures in a row (default 5).
	ConsecutiveFailures int
	// FailureRatio opens the breaker when failures/completed reaches it within Interval (default 0.5; above 1 disables the rule).
	FailureRatio float64
	// MinRequests is the completed requests needed within Interval before FailureRatio applies (default 20).
	MinRequests int
	// Interval is the closed-state window after which counts reset (default 60s).
	Interval time.Duration
	// OpenTimeout is how long the breaker stays open before letting probes through (default 30s).
	OpenTimeout time.Duration
	// HalfOpenRequests is the probes allowed while half-open; that many successes close the breaker (default 1).
	HalfOpenRequests int
	// IsFailure replaces the default classification (transport error, timeout or 5xx); caller cancellation never counts.
	IsFailure func(resp *http.Response, err error) bool
}

func (p BreakerPolicy) validate() error {
	switch {
	case !inUint32(p.ConsecutiveFailures) || !inUint32(p.MinRequests) || !inUint32(p.HalfOpenRequests):
		return errors.New("hermes: BreakerPolicy: ConsecutiveFailures, MinRequests and HalfOpenRequests must be between 0 and 2^32-1")
	case p.FailureRatio < 0 || math.IsNaN(p.FailureRatio):
		return fmt.Errorf("hermes: BreakerPolicy.FailureRatio %v: must be a non-negative number", p.FailureRatio)
	case p.Interval < 0 || p.OpenTimeout < 0:
		return errors.New("hermes: BreakerPolicy: negative duration")
	}
	return nil
}

func inUint32(n int) bool { return n >= 0 && int64(n) <= math.MaxUint32 }

func (p BreakerPolicy) withDefaults() BreakerPolicy {
	if p.ConsecutiveFailures == 0 {
		p.ConsecutiveFailures = 5
	}
	if p.FailureRatio == 0 {
		p.FailureRatio = 0.5
	}
	if p.MinRequests == 0 {
		p.MinRequests = 20
	}
	if p.Interval == 0 {
		p.Interval = 60 * time.Second
	}
	if p.OpenTimeout == 0 {
		p.OpenTimeout = 30 * time.Second
	}
	if p.HalfOpenRequests == 0 {
		p.HalfOpenRequests = 1
	}
	if p.IsFailure == nil {
		p.IsFailure = defaultIsFailure
	}
	return p
}

func defaultIsFailure(resp *http.Response, err error) bool {
	return err != nil || resp.StatusCode >= http.StatusInternalServerError
}

func (p BreakerPolicy) readyToTrip(c gobreaker.Counts) bool {
	if int64(c.ConsecutiveFailures) >= int64(p.ConsecutiveFailures) {
		return true
	}
	completed := int64(c.TotalSuccesses) + int64(c.TotalFailures)
	return completed >= int64(p.MinRequests) && float64(c.TotalFailures)/float64(completed) >= p.FailureRatio
}

// maxBreakerHosts caps the per-host breakers kept by one Client; past it, closed breakers are dropped.
const maxBreakerHosts = 1024

// Outcomes reported to gobreaker, which classifies requests by the error it is given.
var (
	errBreakerFailure  = errors.New("failure")
	errBreakerExcluded = errors.New("excluded")
)

type breakerTransport struct {
	next   http.RoundTripper
	policy BreakerPolicy
	logger func() *slog.Logger
	tel    *telemetry

	mu     sync.Mutex
	byHost map[string]*hostBreaker
}

type hostBreaker struct {
	cb    *gobreaker.TwoStepCircuitBreaker[struct{}]
	attrs []attribute.KeyValue
}

func (t *breakerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	hb := t.breakerFor(ctx, req.URL)
	done, err := hb.cb.Allow()
	if err != nil {
		closeRequestBody(req)
		t.tel.rejections.Add(ctx, 1, metric.WithAttributes(hb.attrs...))
		return nil, circuitOpenError{wrapped{fmt.Errorf("%w for %s", ErrCircuitOpen, hb.cb.Name())}}
	}
	resp, err := t.next.RoundTrip(req)
	done(t.outcome(ctx, resp, err))
	return resp, err
}

func (t *breakerTransport) outcome(ctx context.Context, resp *http.Response, err error) error {
	if _, isToken := errors.AsType[*tokenError](err); isToken || (err != nil && callerCanceled(ctx)) {
		return errBreakerExcluded
	}
	if t.policy.IsFailure(resp, err) {
		return errBreakerFailure
	}
	return nil
}

// breakerFor returns the host's breaker, creating it if needed. When maxBreakerHosts is reached it
// drops the closed breakers, which hold no state worth keeping; if every one is open or half-open,
// the new host gets an untracked breaker so the map stays bounded.
func (t *breakerTransport) breakerFor(ctx context.Context, u *url.URL) *hostBreaker {
	key := breakerKey(u)
	t.mu.Lock()
	defer t.mu.Unlock()
	if hb, ok := t.byHost[key]; ok {
		return hb
	}
	hb := &hostBreaker{attrs: serverAttributes(u)}
	hb.cb = gobreaker.NewTwoStepCircuitBreaker[struct{}](gobreaker.Settings{
		Name:        key,
		MaxRequests: uint32(t.policy.HalfOpenRequests), //nolint:gosec // validated to fit in New
		Interval:    t.policy.Interval,
		Timeout:     t.policy.OpenTimeout,
		ReadyToTrip: t.policy.readyToTrip,
		IsExcluded:  func(err error) bool { return errors.Is(err, errBreakerExcluded) },
		// Runs under the breaker's lock, so it must not call back into the breaker.
		OnStateChange: func(name string, from, to gobreaker.State) { //nolint:contextcheck // gobreaker calls back without a context
			t.logger().Warn("hermes: circuit breaker state changed", "breaker", name, "from", from.String(), "to", to.String())
			// gobreaker's State values are 0 closed, 1 half-open, 2 open: the gauge's documented encoding.
			t.tel.breakerState.Record(context.Background(), int64(to), metric.WithAttributes(hb.attrs...))
		},
	})
	if len(t.byHost) >= maxBreakerHosts {
		maps.DeleteFunc(t.byHost, func(_ string, hb *hostBreaker) bool { return hb.cb.State() == gobreaker.StateClosed })
	}
	if len(t.byHost) < maxBreakerHosts {
		t.tel.breakerState.Record(ctx, int64(gobreaker.StateClosed), metric.WithAttributes(hb.attrs...))
		t.byHost[key] = hb
	}
	return hb
}

// breakerKey is scheme://host:port with the host lowercased and the scheme's default port made explicit.
func breakerKey(u *url.URL) string {
	return u.Scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), strconv.Itoa(port(u)))
}
