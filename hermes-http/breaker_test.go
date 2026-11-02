package hermes_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/svallejo-dev/olympus-core/hermes-http"
)

func TestBreakerOpensFailsFastAndRecovers(t *testing.T) {
	var (
		hits    atomic.Int32
		healthy atomic.Bool
	)
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if !healthy.Load() {
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	c := newClient(t, srv.URL, hermes.WithoutRetry(), hermes.WithCircuitBreaker(hermes.BreakerPolicy{
		ConsecutiveFailures: 2,
		OpenTimeout:         100 * time.Millisecond,
	}))

	for range 2 {
		if code := mustGet(t, c, "/"); code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", code)
		}
	}
	if _, err := get(t, c, "/"); !errors.Is(err, hermes.ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2: an open breaker must not reach the server", hits.Load())
	}

	time.Sleep(150 * time.Millisecond)
	healthy.Store(true)
	for i := range 3 {
		if code := mustGet(t, c, "/"); code != http.StatusOK {
			t.Fatalf("call %d after OpenTimeout: status = %d, want 200", i, code)
		}
	}
	if hits.Load() != 5 {
		t.Errorf("hits = %d, want 5: the half-open probe closes the breaker", hits.Load())
	}
}

func TestBreakerIsPerHost(t *testing.T) {
	down, _ := statusSequence(500)
	up, _ := statusSequence(200)
	srvDown, srvUp := newServer(t, down), newServer(t, up)
	c := newClient(t, srvDown.URL, hermes.WithoutRetry(), hermes.WithCircuitBreaker(hermes.BreakerPolicy{ConsecutiveFailures: 1}))

	mustGet(t, c, "/")
	if _, err := get(t, c, "/"); !errors.Is(err, hermes.ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen for the failing host", err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srvUp.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("healthy host: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthy host status = %d, want 200", resp.StatusCode)
	}
}

func TestBreakerIgnoresClientErrors(t *testing.T) {
	h, hits := statusSequence(404)
	c := newClient(t, newServer(t, h).URL, hermes.WithCircuitBreaker(hermes.BreakerPolicy{ConsecutiveFailures: 2}))

	for range 5 {
		if code := mustGet(t, c, "/"); code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", code)
		}
	}
	if hits.Load() != 5 {
		t.Errorf("hits = %d, want 5: 4xx must not open the breaker", hits.Load())
	}
}

func TestOpenBreakerIsNotRetried(t *testing.T) {
	h, hits := statusSequence(503)
	c := newClient(t, newServer(t, h).URL,
		hermes.WithRetry(hermes.RetryPolicy{MaxAttempts: 5, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
		hermes.WithCircuitBreaker(hermes.BreakerPolicy{ConsecutiveFailures: 2}),
	)

	if _, err := get(t, c, "/"); !errors.Is(err, hermes.ErrCircuitOpen) {
		t.Errorf("err = %v, want ErrCircuitOpen", err)
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2: the retry loop stops once the breaker opens", hits.Load())
	}
}

func TestBreakerCountsDeadlinesButNotCancellation(t *testing.T) {
	tests := []struct {
		name     string
		opts     []hermes.Option
		ctx      func(context.Context) (context.Context, context.CancelFunc)
		wantHits int32
	}{
		{
			name: "caller deadline shorter than the attempt timeout",
			ctx: func(ctx context.Context) (context.Context, context.CancelFunc) {
				return context.WithTimeout(ctx, 50*time.Millisecond)
			},
			wantHits: 2,
		},
		{
			name:     "total timeout shorter than the attempt timeout",
			opts:     []hermes.Option{hermes.WithTimeout(50 * time.Millisecond)},
			wantHits: 2,
		},
		{
			name:     "attempt timeout",
			opts:     []hermes.Option{hermes.WithAttemptTimeout(50 * time.Millisecond)},
			wantHits: 2,
		},
		{
			name: "caller cancellation",
			ctx: func(ctx context.Context) (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(ctx)
				time.AfterFunc(50*time.Millisecond, cancel)
				return ctx, cancel
			},
			wantHits: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := newServer(t, func(_ http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				<-r.Context().Done()
			})
			opts := append([]hermes.Option{hermes.WithoutRetry(), hermes.WithCircuitBreaker(hermes.BreakerPolicy{ConsecutiveFailures: 2})}, tt.opts...)
			c := newClient(t, srv.URL, opts...)

			newCtx := tt.ctx
			if newCtx == nil {
				newCtx = context.WithCancel
			}
			for range 4 {
				ctx, cancel := newCtx(t.Context())
				req, err := c.NewRequest(ctx, http.MethodGet, "/", nil)
				if err != nil {
					t.Fatal(err)
				}
				if resp, err := c.Do(req); err == nil {
					_ = resp.Body.Close()
				}
				cancel()
			}
			if got := hits.Load(); got != tt.wantHits {
				t.Errorf("hits = %d, want %d", got, tt.wantHits)
			}
		})
	}
}
