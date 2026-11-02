package hermes_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/svallejo-dev/olympus-core/hermes-http"
)

func TestAttemptTimeoutIsRetried(t *testing.T) {
	var hits atomic.Int32
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	c := newClient(t, srv.URL, hermes.WithAttemptTimeout(50*time.Millisecond))

	if code := mustGet(t, c, "/"); code != http.StatusOK || hits.Load() != 2 {
		t.Errorf("status = %d, hits = %d; want 200 after 2 attempts", code, hits.Load())
	}
}

func TestTotalTimeoutBoundsTheCall(t *testing.T) {
	var hits atomic.Int32
	srv := newServer(t, func(_ http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-r.Context().Done()
	})
	c := newClient(t, srv.URL,
		hermes.WithAttemptTimeout(30*time.Millisecond),
		hermes.WithTimeout(150*time.Millisecond),
		hermes.WithRetry(hermes.RetryPolicy{MaxAttempts: 100, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
		hermes.WithoutCircuitBreaker(),
	)

	start := time.Now()
	resp, err := get(t, c, "/")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("Do succeeded, want a timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("elapsed = %v, want about 150ms", elapsed)
	}
	if n := hits.Load(); n < 2 || n > 10 {
		t.Errorf("hits = %d, want a few attempt timeouts within the total timeout", n)
	}
}

func TestBodyReadableAfterDo(t *testing.T) {
	payload := bytes.Repeat([]byte("hermes-"), 40_000)

	t.Run("large body", func(t *testing.T) {
		srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(payload) })
		c := newClient(t, srv.URL)
		assertBody(t, c, payload)
	})

	t.Run("slow body outlives the attempt timeout", func(t *testing.T) {
		srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(payload[:1024])
			w.(http.Flusher).Flush()
			time.Sleep(150 * time.Millisecond)
			_, _ = w.Write(payload[1024:])
		})
		c := newClient(t, srv.URL, hermes.WithAttemptTimeout(50*time.Millisecond))
		assertBody(t, c, payload)
	})
}

func assertBody(t *testing.T, c *hermes.Client, want []byte) {
	t.Helper()
	resp, err := get(t, c, "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body after Do: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("read %d bytes, want %d", len(got), len(want))
	}
}

func TestAttemptTimeoutWrapsDeadlineExceeded(t *testing.T) {
	srv := newServer(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	c := newClient(t, srv.URL, hermes.WithoutRetry(), hermes.WithAttemptTimeout(50*time.Millisecond))

	resp, err := get(t, c, "/")
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

func TestTotalTimeoutCoversRedirects(t *testing.T) {
	slow := func(r *http.Request, then func()) {
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
			then()
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) {
		slow(r, func() { http.Redirect(w, r, "/b", http.StatusFound) })
	})
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) {
		slow(r, func() { _, _ = w.Write([]byte("ok")) })
	})
	srv := newServer(t, mux.ServeHTTP)
	c := newClient(t, srv.URL, hermes.WithTimeout(300*time.Millisecond), hermes.WithoutRetry())

	send := map[string]func(*http.Request) (*http.Response, error){
		"Client.Do":  c.Do,
		"HTTPClient": c.HTTPClient().Do,
	}
	for name, do := range send {
		t.Run(name, func(t *testing.T) {
			req, err := c.NewRequest(t.Context(), http.MethodGet, "/a", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("err = %v, want the 300ms total timeout to cover both 200ms hops", err)
			}
		})
	}
}
