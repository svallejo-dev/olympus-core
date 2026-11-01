package hermes_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/svallejo-dev/olympus/hermes"
)

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name string
		url  string
		opts []hermes.Option
	}{
		{"empty URL", "", nil},
		{"relative URL", "/v1", nil},
		{"unsupported scheme", "ftp://example.com", nil},
		{"missing host", "https://", nil},
		{"negative timeout", "https://example.com", []hermes.Option{hermes.WithTimeout(-time.Second)}},
		{"negative attempt timeout", "https://example.com", []hermes.Option{hermes.WithAttemptTimeout(-time.Second)}},
		{"negative attempts", "https://example.com", []hermes.Option{hermes.WithRetry(hermes.RetryPolicy{MaxAttempts: -1})}},
		{"negative failure ratio", "https://example.com", []hermes.Option{hermes.WithCircuitBreaker(hermes.BreakerPolicy{FailureRatio: -1})}},
		{"negative consecutive failures", "https://example.com", []hermes.Option{hermes.WithCircuitBreaker(hermes.BreakerPolicy{ConsecutiveFailures: -1})}},
		{"negative min requests", "https://example.com", []hermes.Option{hermes.WithCircuitBreaker(hermes.BreakerPolicy{MinRequests: -1})}},
		{"negative half-open requests", "https://example.com", []hermes.Option{hermes.WithCircuitBreaker(hermes.BreakerPolicy{HalfOpenRequests: -1})}},
		{"negative max Retry-After", "https://example.com", []hermes.Option{hermes.WithRetry(hermes.RetryPolicy{MaxRetryAfter: -1})}},
		{"zero concurrency", "https://example.com", []hermes.Option{hermes.WithMaxConcurrent(0)}},
		{"zero rate", "https://example.com", []hermes.Option{hermes.WithRateLimit(0, 1)}},
		{"zero burst", "https://example.com", []hermes.Option{hermes.WithRateLimit(1, 0)}},
		{"nil transport", "https://example.com", []hermes.Option{hermes.WithTransport(nil)}},
		{"nil token", "https://example.com", []hermes.Option{hermes.WithBearerToken(nil)}},
		{"zero response cap", "https://example.com", []hermes.Option{hermes.WithMaxResponseBytes(0)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := hermes.New(tt.url, tt.opts...); err == nil {
				t.Errorf("New(%q) succeeded, want an error", tt.url)
			}
		})
	}
}

func TestNewRequestResolvesPathAgainstBaseURL(t *testing.T) {
	tests := []struct {
		base, path, want string
	}{
		{"https://api.test/v1", "/users", "https://api.test/v1/users"},
		{"https://api.test/v1/", "users", "https://api.test/v1/users"},
		{"https://api.test", "/users?active=true&page=2", "https://api.test/users?active=true&page=2"},
		{"https://api.test/v1?key=abc", "/users?page=2", "https://api.test/v1/users?key=abc&page=2"},
		{"https://api.test/v1", "", "https://api.test/v1"},
		{"https://api.test/v1", "?page=2", "https://api.test/v1?page=2"},
		{"https://api.test/v1", "/files/a%2Fb", "https://api.test/v1/files/a%2Fb"},
	}
	for _, tt := range tests {
		t.Run(tt.base+" + "+tt.path, func(t *testing.T) {
			c, err := hermes.New(tt.base)
			if err != nil {
				t.Fatal(err)
			}
			req, err := c.NewRequest(t.Context(), http.MethodGet, tt.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := req.URL.String(); got != tt.want {
				t.Errorf("URL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewRequestRejectsAbsolutePath(t *testing.T) {
	c, err := hermes.New("https://api.test/v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.NewRequest(t.Context(), http.MethodGet, "https://other.test/x", nil); err == nil {
		t.Error("NewRequest with an absolute URL succeeded, want an error")
	}
}

func TestDefaultHeadersAndUserAgent(t *testing.T) {
	got := make(chan http.Header, 1)
	srv := newServer(t, func(_ http.ResponseWriter, r *http.Request) { got <- r.Header.Clone() })
	c := newClient(t, srv.URL,
		hermes.WithUserAgent("billing/1.2"),
		hermes.WithHeader("X-Api-Key", "default"),
		hermes.WithHeader("X-Tenant", "acme"),
	)

	if _, err := hermes.GetJSON[struct{}](t.Context(), c, "/", hermes.Header("X-Tenant", "override")); err != nil {
		t.Fatal(err)
	}
	h := <-got
	for key, want := range map[string]string{
		"User-Agent": "billing/1.2",
		"X-Api-Key":  "default",
		"X-Tenant":   "override",
		"Accept":     "application/json",
	} {
		if v := h.Get(key); v != want {
			t.Errorf("%s = %q, want %q", key, v, want)
		}
	}
}

func TestBearerTokenIsFetchedPerAttempt(t *testing.T) {
	var (
		mu    sync.Mutex
		auths []string
		calls atomic.Int32
	)
	codes, _ := statusSequence(503, 200)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		codes(w, r)
	})
	c := newClient(t, srv.URL, hermes.WithBearerToken(func(context.Context) (string, error) {
		return fmt.Sprintf("tok-%d", calls.Add(1)), nil
	}))

	mustGet(t, c, "/")
	if want := []string{"Bearer tok-1", "Bearer tok-2"}; !slices.Equal(auths, want) {
		t.Errorf("Authorization headers = %q, want %q", auths, want)
	}
}

func TestBearerTokenErrorIsNotRetried(t *testing.T) {
	h, hits := statusSequence(200)
	errNoToken := errors.New("token endpoint down")
	var calls atomic.Int32
	c := newClient(t, newServer(t, h).URL, hermes.WithBearerToken(func(context.Context) (string, error) {
		calls.Add(1)
		return "", errNoToken
	}))

	if _, err := get(t, c, "/"); !errors.Is(err, errNoToken) {
		t.Errorf("err = %v, want the token error", err)
	}
	if calls.Load() != 1 || hits.Load() != 0 {
		t.Errorf("token calls = %d, server hits = %d; want 1 and 0", calls.Load(), hits.Load())
	}
}

func TestClientIsSafeForConcurrentUse(t *testing.T) {
	h, hits := statusSequence(200)
	c := newClient(t, newServer(t, h).URL)

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := hermes.GetJSON[map[string]int](t.Context(), c, "/"); err != nil {
				t.Errorf("GetJSON: %v", err)
			}
		})
	}
	wg.Wait()
	if hits.Load() != 20 {
		t.Errorf("hits = %d, want 20", hits.Load())
	}
}

func TestBearerTokenErrorIsNotRetriedWithCustomShouldRetry(t *testing.T) {
	h, hits := statusSequence(200)
	var calls atomic.Int32
	c := newClient(t, newServer(t, h).URL,
		hermes.WithBearerToken(func(context.Context) (string, error) {
			calls.Add(1)
			return "", errors.New("token endpoint down")
		}),
		hermes.WithRetry(hermes.RetryPolicy{
			InitialBackoff: time.Millisecond,
			MaxBackoff:     time.Millisecond,
			ShouldRetry:    func(resp *http.Response, err error) bool { return err != nil || resp.StatusCode >= 500 },
		}),
	)

	if _, err := get(t, c, "/"); err == nil {
		t.Error("Do succeeded, want the token error")
	}
	if calls.Load() != 1 || hits.Load() != 0 {
		t.Errorf("token calls = %d, server hits = %d; want 1 and 0", calls.Load(), hits.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportErrorRedactsURL(t *testing.T) {
	failing := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") })
	c := newClient(t, "http://user:secret@api.test", hermes.WithTransport(failing), hermes.WithoutRetry())

	_, jsonErr := hermes.GetJSON[struct{}](t.Context(), c, "/orders?token=s3cr3t")
	_, doErr := get(t, c, "/orders?token=s3cr3t")
	for name, err := range map[string]error{"GetJSON": jsonErr, "Do": doErr} {
		urlErr, ok := errors.AsType[*url.Error](err)
		if !ok {
			t.Fatalf("%s: err = %v, want a *url.Error", name, err)
		}
		if urlErr.URL != "http://api.test/orders" {
			t.Errorf("%s: url.Error.URL = %q, want http://api.test/orders", name, urlErr.URL)
		}
		for _, leak := range []string{"user", "secret", "token", "s3cr3t"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: Error() = %q leaks %q", name, err, leak)
			}
		}
	}
}

func TestNilResponseBodyIsReplaced(t *testing.T) {
	noBody := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Request: r}, nil
	})
	c := newClient(t, "http://api.test", hermes.WithTransport(noBody), hermes.WithMaxConcurrent(1))

	if code := mustGet(t, c, "/"); code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", code)
	}
	if _, err := hermes.GetJSON[struct{}](t.Context(), c, "/"); err != nil {
		t.Errorf("GetJSON: %v", err)
	}
}

type idleCountingTransport struct {
	http.RoundTripper
	closes atomic.Int32
}

func (t *idleCountingTransport) CloseIdleConnections() { t.closes.Add(1) }

func TestCloseIdleConnectionsReachesBaseTransport(t *testing.T) {
	base := &idleCountingTransport{RoundTripper: http.DefaultTransport}
	c := newClient(t, "http://api.test", hermes.WithTransport(base))

	c.CloseIdleConnections()
	c.HTTPClient().CloseIdleConnections()
	if n := base.closes.Load(); n != 2 {
		t.Errorf("base CloseIdleConnections calls = %d, want 2", n)
	}
}
