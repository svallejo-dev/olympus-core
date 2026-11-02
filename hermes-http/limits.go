package hermes

import (
	"context"
	"fmt"
	"net/http"

	"golang.org/x/time/rate"
)

// errRateLimitDeadline is returned when the next token would arrive after the request's deadline.
var errRateLimitDeadline = fmt.Errorf("hermes: rate limit wait would exceed the deadline: %w", context.DeadlineExceeded)

// rateLimitTransport sits below the retry layer, so every attempt waits for its own token.
type rateLimitTransport struct {
	next    http.RoundTripper
	limiter *rate.Limiter
}

func (t *rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if err := t.limiter.Wait(ctx); err != nil {
		closeRequestBody(req)
		if ctx.Err() == nil {
			return nil, errRateLimitDeadline
		}
		return nil, fmt.Errorf("hermes: rate limit: %w", err)
	}
	return t.next.RoundTrip(req)
}

// bulkheadTransport is a counting semaphore; a slot is held until the response body is released.
type bulkheadTransport struct {
	next  http.RoundTripper
	slots chan struct{}
}

func (t *bulkheadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	select {
	case t.slots <- struct{}{}:
	case <-ctx.Done():
		closeRequestBody(req)
		return nil, fmt.Errorf("hermes: wait for concurrency slot: %w", context.Cause(ctx))
	}
	release := func() { <-t.slots }
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		release()
		return nil, err
	}
	resp.Body = releaseOnClose(resp.Body, release)
	return resp, nil
}
