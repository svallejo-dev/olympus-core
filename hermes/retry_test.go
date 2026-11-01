package hermes_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/svallejo-dev/olympus-core/hermes"
)

func TestRetrySucceedsAfterTransientFailures(t *testing.T) {
	h, hits := statusSequence(503, 503, 200)
	c := newClient(t, newServer(t, h).URL)

	got, err := hermes.GetJSON[struct{ Attempt int }](t.Context(), c, "/")
	if err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if got.Attempt != 3 || hits.Load() != 3 {
		t.Errorf("attempt = %d, hits = %d; want 3 and 3", got.Attempt, hits.Load())
	}
}

func TestRetryExhaustedReturnsLastResponse(t *testing.T) {
	h, hits := statusSequence(503)
	c := newClient(t, newServer(t, h).URL, hermes.WithoutCircuitBreaker())

	if code := mustGet(t, c, "/"); code != http.StatusServiceUnavailable {
		t.Errorf("Do status = %d, want 503", code)
	}
	if hits.Load() != 3 {
		t.Errorf("hits = %d, want 3", hits.Load())
	}

	_, err := hermes.GetJSON[map[string]any](t.Context(), c, "/")
	statusErr, ok := errors.AsType[*hermes.StatusError](err)
	if !ok || statusErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GetJSON err = %v, want *StatusError 503", err)
	}
	if hits.Load() != 6 {
		t.Errorf("hits = %d, want 6", hits.Load())
	}
}

func TestRetryNonIdempotentRequests(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	codes, hits := statusSequence(503, 503, 200)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		codes(w, r)
	})
	in := map[string]string{"order": "42"}

	t.Run("without idempotency key", func(t *testing.T) {
		hits.Store(0)
		c := newClient(t, srv.URL)
		_, err := hermes.PostJSON[map[string]any](t.Context(), c, "/orders", in)
		if _, ok := errors.AsType[*hermes.StatusError](err); !ok || hits.Load() != 1 {
			t.Errorf("err = %v, hits = %d; want StatusError after 1 attempt", err, hits.Load())
		}
	})

	t.Run("with idempotency key", func(t *testing.T) {
		hits.Store(0)
		bodies = nil
		c := newClient(t, srv.URL)
		_, err := hermes.PostJSON[map[string]any](t.Context(), c, "/orders", in, hermes.IdempotencyKey("k-1"))
		if err != nil || hits.Load() != 3 {
			t.Fatalf("err = %v, hits = %d; want success after 3 attempts", err, hits.Load())
		}
		want := `{"order":"42"}`
		if !slices.Equal(bodies, []string{want, want, want}) {
			t.Errorf("bodies = %q, want the same body three times", bodies)
		}
	})

	t.Run("RetryNonIdempotent", func(t *testing.T) {
		hits.Store(0)
		c := newClient(t, srv.URL, hermes.WithRetry(hermes.RetryPolicy{
			InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond, RetryNonIdempotent: true,
		}))
		if _, err := hermes.PostJSON[map[string]any](t.Context(), c, "/orders", in); err != nil || hits.Load() != 3 {
			t.Errorf("err = %v, hits = %d; want success after 3 attempts", err, hits.Load())
		}
	})
}

func TestNoRetryWithoutReplayableBody(t *testing.T) {
	h, hits := statusSequence(503, 200)
	c := newClient(t, newServer(t, h).URL)

	req, err := c.NewRequest(t.Context(), http.MethodPut, "/", io.NopCloser(strings.NewReader("data")))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || hits.Load() != 1 {
		t.Errorf("status = %d, hits = %d; want 503 after 1 attempt", resp.StatusCode, hits.Load())
	}
}

func TestRetryAfterIsHonoured(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	c := newClient(t, srv.URL)

	start := time.Now()
	if code := mustGet(t, c, "/"); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("elapsed = %v, want at least the 1s Retry-After", elapsed)
	}
}

func TestRetryAfterBeyondDeadlineStopsEarly(t *testing.T) {
	var hits atomic.Int32
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	c := newClient(t, srv.URL, hermes.WithTimeout(2*time.Second))

	start := time.Now()
	if code := mustGet(t, c, "/"); code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want the last response, 503", code)
	}
	if elapsed := time.Since(start); elapsed > time.Second || hits.Load() != 1 {
		t.Errorf("elapsed = %v, hits = %d; want an immediate return after 1 attempt", elapsed, hits.Load())
	}
}

func TestCallerCancelStopsWithoutRetry(t *testing.T) {
	var hits atomic.Int32
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-r.Context().Done()
	})
	c := newClient(t, srv.URL)

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)
	req, err := c.NewRequest(ctx, http.MethodGet, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second || hits.Load() != 1 {
		t.Errorf("elapsed = %v, hits = %d; want a prompt return after 1 attempt", elapsed, hits.Load())
	}
}

func TestShouldRetryOverridesClassification(t *testing.T) {
	h, hits := statusSequence(500, 200)
	c := newClient(t, newServer(t, h).URL, hermes.WithRetry(hermes.RetryPolicy{
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		ShouldRetry:    func(resp *http.Response, err error) bool { return err != nil || resp.StatusCode == 500 },
	}))

	if code := mustGet(t, c, "/"); code != http.StatusOK || hits.Load() != 2 {
		t.Errorf("status = %d, hits = %d; want 200 after 2 attempts", code, hits.Load())
	}
}

func TestWithoutRetrySendsOnce(t *testing.T) {
	h, hits := statusSequence(503, 200)
	c := newClient(t, newServer(t, h).URL, hermes.WithoutRetry())

	if code := mustGet(t, c, "/"); code != http.StatusServiceUnavailable || hits.Load() != 1 {
		t.Errorf("status = %d, hits = %d; want 503 after 1 attempt", code, hits.Load())
	}
}

func TestRetryReplaysBodyThroughHTTPClient(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies [][]byte
	)
	codes, _ := statusSequence(502, 200)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		codes(w, r)
	})
	c := newClient(t, srv.URL)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/items/1", bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Errorf("status = %d, bodies = %q; want 200 with the body sent twice", resp.StatusCode, bodies)
	}
}

func TestRetryAfterBeyondMaxRetryAfterStopsRetrying(t *testing.T) {
	tests := []struct {
		name       string
		retryAfter string
		policy     hermes.RetryPolicy
	}{
		{"default 30s cap", "60", hermes.RetryPolicy{}},
		{"custom cap", "2", hermes.RetryPolicy{MaxRetryAfter: time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.Header().Set("Retry-After", tt.retryAfter)
				w.WriteHeader(http.StatusServiceUnavailable)
			})
			c := newClient(t, srv.URL, hermes.WithTimeout(0), hermes.WithRetry(tt.policy))

			start := time.Now()
			if code := mustGet(t, c, "/"); code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want the last response, 503", code)
			}
			if elapsed := time.Since(start); elapsed > 500*time.Millisecond || hits.Load() != 1 {
				t.Errorf("elapsed = %v, hits = %d; want an immediate return after 1 attempt", elapsed, hits.Load())
			}
		})
	}
}
