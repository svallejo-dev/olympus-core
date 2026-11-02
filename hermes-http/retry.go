package hermes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// RetryPolicy configures retries with exponential backoff and full jitter; zero fields take the defaults.
type RetryPolicy struct {
	// MaxAttempts counts the first attempt too (default 3).
	MaxAttempts int
	// InitialBackoff caps the first jittered wait and doubles on every retry (default 100ms).
	InitialBackoff time.Duration
	// MaxBackoff caps the computed backoff (default 2s); a Retry-After header may ask for longer, up to MaxRetryAfter.
	MaxBackoff time.Duration
	// MaxRetryAfter is the longest Retry-After honoured (default 30s); a longer one stops retrying and returns that response.
	MaxRetryAfter time.Duration
	// RetryNonIdempotent also retries every POST, PATCH and other method that no rule below marks as idempotent.
	RetryNonIdempotent bool
	// IdempotencyHeader names the header whose presence makes a POST or PATCH retryable (default Idempotency-Key).
	IdempotencyHeader string
	// IsIdempotent adds a rule for POST, PATCH and other methods, e.g. by path or query; it must not consume the body.
	IsIdempotent func(req *http.Request) bool
	// ShouldRetry replaces the default classification (transport errors and 429, 502, 503, 504); resp is nil when err is not.
	// Breaker rejections, bearer-token failures and rate limit waits beyond the deadline are never retried, whatever it returns.
	ShouldRetry func(resp *http.Response, err error) bool
}

const defaultIdempotencyHeader = "Idempotency-Key"

func (p RetryPolicy) validate() error {
	if p.MaxAttempts < 0 || p.InitialBackoff < 0 || p.MaxBackoff < 0 || p.MaxRetryAfter < 0 {
		return errors.New("hermes: RetryPolicy: negative MaxAttempts, backoff or MaxRetryAfter")
	}
	return nil
}

func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.MaxAttempts == 0 {
		p.MaxAttempts = 3
	}
	if p.InitialBackoff == 0 {
		p.InitialBackoff = 100 * time.Millisecond
	}
	if p.MaxBackoff == 0 {
		p.MaxBackoff = 2 * time.Second
	}
	if p.MaxRetryAfter == 0 {
		p.MaxRetryAfter = 30 * time.Second
	}
	if p.IdempotencyHeader == "" {
		p.IdempotencyHeader = defaultIdempotencyHeader
	}
	if p.ShouldRetry == nil {
		p.ShouldRetry = defaultShouldRetry
	}
	return p
}

func defaultShouldRetry(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// backoff returns Retry-After when the response carries one, otherwise a full-jitter exponential wait;
// ok is false when Retry-After exceeds MaxRetryAfter.
func (p RetryPolicy) backoff(attempt int, resp *http.Response) (wait time.Duration, ok bool) {
	if resp != nil {
		if d, found := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); found {
			return d, d <= p.MaxRetryAfter
		}
	}
	ceiling := p.InitialBackoff
	for range attempt - 1 {
		if ceiling >= p.MaxBackoff/2 {
			break
		}
		ceiling *= 2
	}
	ceiling = min(ceiling, p.MaxBackoff)
	return rand.N(ceiling + 1), true //nolint:gosec // jitter does not need a cryptographic source
}

// parseRetryAfter reads delay-seconds or an HTTP-date (RFC 9110 §10.2.3).
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(min(secs, int64(math.MaxInt64/time.Second))) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}

type retryTransport struct {
	next   http.RoundTripper
	policy RetryPolicy
	logger func() *slog.Logger
	tel    *telemetry
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.policy.allows(req) || !rewindable(req) {
		return t.next.RoundTrip(req)
	}
	ctx := req.Context()
	attemptReq := req
	for attempt := 1; ; attempt++ {
		resp, err := t.next.RoundTrip(attemptReq)
		reason, retry := t.shouldRetry(ctx, resp, err)
		if !retry || attempt == t.policy.MaxAttempts {
			return resp, err
		}
		wait, ok := t.policy.backoff(attempt, resp)
		if !ok {
			return resp, err
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait {
			return resp, err
		}
		if resp != nil {
			drainAndClose(resp)
		}
		t.record(ctx, req, attempt, reason, wait)
		if err := sleep(ctx, wait); err != nil {
			return nil, retryWaitError{wrapped{fmt.Errorf("hermes: wait to retry: %w", err)}}
		}
		if attemptReq, err = rewind(req); err != nil {
			return nil, err
		}
	}
}

func (t *retryTransport) shouldRetry(ctx context.Context, resp *http.Response, err error) (reason string, ok bool) {
	if ctx.Err() != nil || neverRetried(err) || !t.policy.ShouldRetry(resp, err) {
		return "", false
	}
	switch {
	case errors.Is(err, errAttemptTimeout):
		return "timeout", true
	case err != nil:
		return "transport_error", true
	default:
		return strconv.Itoa(resp.StatusCode), true
	}
}

// neverRetried reports failures that retrying within the call cannot fix, whatever ShouldRetry says.
func neverRetried(err error) bool {
	_, isToken := errors.AsType[*tokenError](err)
	return isToken || errors.Is(err, ErrCircuitOpen) || errors.Is(err, errRateLimitDeadline)
}

func (t *retryTransport) record(ctx context.Context, req *http.Request, attempt int, reason string, wait time.Duration) {
	attrs := append(serverAttributes(req.URL), retryReasonKey.String(reason))
	t.tel.retries.Add(ctx, 1, metric.WithAttributes(attrs...))
	trace.SpanFromContext(ctx).AddEvent("hermes.retry", trace.WithAttributes(
		semconv.HTTPRequestResendCount(attempt),
		retryReasonKey.String(reason),
		attribute.Int64("hermes.retry.delay_ms", wait.Milliseconds()),
	))
	t.logger().DebugContext(ctx, "hermes: retrying request",
		"method", req.Method, "url", redactURL(req.URL), "attempt", attempt+1, "reason", reason, "delay", wait)
}

// allows reports whether req is safe to send more than once.
func (p RetryPolicy) allows(req *http.Request) bool {
	switch req.Method {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete, http.MethodTrace:
		return true
	}
	return p.RetryNonIdempotent ||
		markedIdempotent(req.Context()) ||
		req.Header.Get(p.IdempotencyHeader) != "" ||
		(p.IsIdempotent != nil && p.IsIdempotent(req))
}

func hasBody(req *http.Request) bool {
	return req.Body != nil && req.Body != http.NoBody
}

func rewindable(req *http.Request) bool {
	return !hasBody(req) || req.GetBody != nil
}

// rewind clones req for a new attempt with a fresh body, since the previous attempt consumed it.
func rewind(req *http.Request) (*http.Request, error) {
	r := req.Clone(req.Context())
	if hasBody(req) {
		body, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("hermes: rewind request body: %w", err)
		}
		r.Body = body
	}
	return r, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}
