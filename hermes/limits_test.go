package hermes_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/svallejo-dev/olympus/hermes"
)

func TestBulkheadLimitsConcurrency(t *testing.T) {
	var inFlight, peak atomic.Int32
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	})
	c := newClient(t, srv.URL, hermes.WithMaxConcurrent(2))

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			resp, err := get(t, c, "/")
			if err != nil {
				t.Errorf("GET: %v", err)
				return
			}
			_ = resp.Body.Close()
		})
	}
	wg.Wait()
	if p := peak.Load(); p != 2 {
		t.Errorf("peak concurrency = %d, want 2", p)
	}
}

func TestBulkheadWaitHonoursContext(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	srv := newServer(t, func(_ http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		<-release
	})
	c := newClient(t, srv.URL, hermes.WithMaxConcurrent(1))

	var wg sync.WaitGroup
	wg.Go(func() {
		if code := mustGet(t, c, "/"); code != http.StatusOK {
			t.Errorf("first call status = %d, want 200", code)
		}
	})
	<-arrived

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	req, err := c.NewRequest(ctx, http.MethodGet, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded while waiting for a slot", err)
	}
	close(release)
	wg.Wait()
}

func TestRateLimiterPacesCalls(t *testing.T) {
	h, _ := statusSequence(200)
	c := newClient(t, newServer(t, h).URL, hermes.WithRateLimit(20, 1))

	start := time.Now()
	for range 3 {
		mustGet(t, c, "/")
	}
	// One token up front, then one every 50ms.
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Errorf("3 calls took %v, want at least ~100ms at 20/s with burst 1", elapsed)
	}
}

func TestRateLimiterHonoursContext(t *testing.T) {
	h, hits := statusSequence(200)
	c := newClient(t, newServer(t, h).URL, hermes.WithRateLimit(0.5, 1))
	mustGet(t, c, "/")

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	req, err := c.NewRequest(ctx, http.MethodGet, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("Do succeeded, want a rate limit error")
	}
	if elapsed := time.Since(start); elapsed > time.Second || hits.Load() != 1 {
		t.Errorf("elapsed = %v, hits = %d; want a prompt error without reaching the server", elapsed, hits.Load())
	}
}

func TestRateLimiterPacesRetries(t *testing.T) {
	h, hits := statusSequence(503)
	c := newClient(t, newServer(t, h).URL, hermes.WithRateLimit(20, 1), hermes.WithoutCircuitBreaker())

	start := time.Now()
	mustGet(t, c, "/")
	// Three attempts: one token up front, then one every 50ms.
	if elapsed := time.Since(start); hits.Load() != 3 || elapsed < 90*time.Millisecond {
		t.Errorf("hits = %d in %v, want 3 attempts paced over at least ~100ms", hits.Load(), elapsed)
	}
}

func TestRateLimiterDeadlineErrorWrapsDeadlineExceeded(t *testing.T) {
	h, _ := statusSequence(200)
	c := newClient(t, newServer(t, h).URL, hermes.WithRateLimit(0.5, 1))
	mustGet(t, c, "/")

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	req, err := c.NewRequest(ctx, http.MethodGet, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
}
