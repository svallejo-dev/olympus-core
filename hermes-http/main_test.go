package hermes_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/svallejo-dev/olympus-core/hermes-http"
)

// TestMain fails the run when goroutines started by the tests outlive them. It avoids a goleak
// dependency: with every httptest server closed, the count must return to the baseline.
func TestMain(m *testing.M) {
	baseline := runtime.NumGoroutine()
	code := m.Run()
	if code == 0 {
		if err := waitForGoroutines(baseline, 5*time.Second); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	os.Exit(code)
}

func waitForGoroutines(baseline int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		n := runtime.NumGoroutine()
		if n <= baseline {
			return nil
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			buf = buf[:runtime.Stack(buf, true)]
			return fmt.Errorf("goroutine leak: %d running, baseline %d\n%s", n, baseline, buf)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// newClient builds a quiet Client whose retries wait at most 1ms, keeping the suite fast; opts may override both.
func newClient(t *testing.T, baseURL string, opts ...hermes.Option) *hermes.Client {
	t.Helper()
	defaults := []hermes.Option{
		hermes.WithRetry(hermes.RetryPolicy{InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
		hermes.WithLogger(slog.New(slog.DiscardHandler)),
	}
	c, err := hermes.New(baseURL, append(defaults, opts...)...)
	if err != nil {
		t.Fatalf("New(%q): %v", baseURL, err)
	}
	return c
}

// statusSequence answers the n-th request with codes[n], repeating the last code, and counts requests.
func statusSequence(codes ...int) (http.HandlerFunc, *atomic.Int32) {
	var hits atomic.Int32
	return func(w http.ResponseWriter, _ *http.Request) {
		n := int(hits.Add(1))
		code := codes[min(n, len(codes))-1]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, `{"attempt":%d}`, n)
	}, &hits
}

func get(t *testing.T, c *hermes.Client, path string) (*http.Response, error) {
	t.Helper()
	req, err := c.NewRequest(t.Context(), http.MethodGet, path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return c.Do(req)
}

// mustGet performs a GET that must not fail at the transport level and returns its drained status.
func mustGet(t *testing.T, c *hermes.Client, path string) int {
	t.Helper()
	resp, err := get(t, c, path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}
