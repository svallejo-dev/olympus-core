package hermes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// errAttemptTimeout wraps context.DeadlineExceeded so an attempt timeout is detected like any other deadline.
var errAttemptTimeout = fmt.Errorf("hermes: attempt timed out: %w", context.DeadlineExceeded)

// attemptTimeoutTransport bounds one attempt until its response headers arrive; once they do,
// the body read is bounded by the total timeout only, so a slow download is not cut mid-stream.
// The attempt context derives from the request's, so it never outlives the caller's or the total deadline.
type attemptTimeoutTransport struct {
	next    http.RoundTripper
	timeout time.Duration
}

func (t *attemptTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	timer := time.AfterFunc(t.timeout, func() { cancel(errAttemptTimeout) })
	resp, err := t.next.RoundTrip(req.WithContext(ctx))
	if !timer.Stop() {
		if err == nil {
			_ = resp.Body.Close()
		}
		cancel(nil)
		return nil, fmt.Errorf("%w after %v", errAttemptTimeout, t.timeout)
	}
	if err != nil {
		cancel(nil)
		return nil, err
	}
	resp.Body = releaseOnClose(resp.Body, func() { cancel(nil) })
	return resp, nil
}

// callerCanceled reports whether the caller cancelled ctx; a passed deadline (the caller's, the
// total or the attempt timeout) is not a cancellation.
func callerCanceled(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled) && !errors.Is(context.Cause(ctx), errAttemptTimeout)
}
