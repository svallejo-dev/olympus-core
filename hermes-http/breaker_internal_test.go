package hermes

import (
	"fmt"
	"log/slog"
	"net/url"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"
)

func newTestBreakerGroup(t *testing.T) *breakerGroup {
	t.Helper()
	tel, err := newTelemetry(noop.NewMeterProvider(), "")
	if err != nil {
		t.Fatal(err)
	}
	return &breakerGroup{
		policy: BreakerPolicy{ConsecutiveFailures: 1}.withDefaults(),
		logger: func() *slog.Logger { return slog.New(slog.DiscardHandler) },
		tel:    tel,
		byHost: make(map[string]*hostBreaker),
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestBreakerKeySharesDefaultPorts(t *testing.T) {
	bt := newTestBreakerGroup(t)
	for _, pair := range [][2]string{
		{"https://api.test/a", "https://API.test:443/b"},
		{"http://api.test", "http://api.test:80"},
		{"http://[::1]/", "http://[::1]:80/"},
	} {
		a, b := bt.breakerFor(t.Context(), mustParseURL(t, pair[0])), bt.breakerFor(t.Context(), mustParseURL(t, pair[1]))
		if a != b {
			t.Errorf("%s and %s use different breakers, want one", pair[0], pair[1])
		}
	}
	if a, b := bt.breakerFor(t.Context(), mustParseURL(t, "http://api.test")), bt.breakerFor(t.Context(), mustParseURL(t, "https://api.test")); a == b {
		t.Error("http and https share a breaker, want one per scheme")
	}
}

func TestBreakerMapIsBounded(t *testing.T) {
	bt := newTestBreakerGroup(t)
	open := bt.breakerFor(t.Context(), mustParseURL(t, "https://down.test"))
	done, err := open.cb.Allow()
	if err != nil {
		t.Fatal(err)
	}
	done(errBreakerFailure)

	for i := range 3 * maxBreakerHosts {
		bt.breakerFor(t.Context(), mustParseURL(t, fmt.Sprintf("https://host-%d.test", i)))
	}
	if n := len(bt.byHost); n > maxBreakerHosts {
		t.Errorf("breakers = %d, want at most %d", n, maxBreakerHosts)
	}
	if got := bt.breakerFor(t.Context(), mustParseURL(t, "https://down.test")); got != open {
		t.Error("the open breaker was dropped, want it kept")
	}
}

func TestEndpointSharesTheBaseTransport(t *testing.T) {
	c, err := New("http://api.test")
	if err != nil {
		t.Fatal(err)
	}
	e, err := c.Endpoint("e", WithoutRetry())
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.transport != e.cfg.transport {
		t.Error("endpoint has its own base transport, want the parent's connection pool")
	}
	if c.cfg.breakers != e.cfg.breakers {
		t.Error("endpoint without WithCircuitBreaker has its own breakers, want the parent's")
	}
}
