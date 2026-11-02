// Package hermes calls HTTP APIs with retries, timeouts, circuit breaking, limits and OpenTelemetry built in.
package hermes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

// Client is safe for concurrent use and immutable after New; its only state is an in-memory circuit breaker per scheme, host and port.
type Client struct {
	base *url.URL
	// client has no Jar, so no session is kept between requests; its Timeout is the total timeout.
	client           *http.Client
	maxResponseBytes int64
	// cfg is what Endpoint derives from; nothing changes it after the Client is built.
	cfg config
}

// New returns a Client for the absolute http or https baseURL; it fails on an invalid URL or option.
func New(baseURL string, opts ...Option) (*Client, error) {
	base, err := parseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	return newClient(base, newConfig(opts))
}

// Endpoint returns a Client with the same base URL and configuration, except for opts, under a fixed name that labels its telemetry; create it once at startup and reuse it.
// A breaker, rate limit or concurrency cap passed in opts has its own state; one left out is shared with c. Connections are always shared unless opts include WithTransport.
func (c *Client) Endpoint(name string, opts ...Option) (*Client, error) {
	if name == "" {
		return nil, errors.New("hermes: Endpoint: empty name")
	}
	cfg := c.cfg
	cfg.name, cfg.headers = name, c.cfg.headers.Clone()
	for _, opt := range opts {
		opt(&cfg)
	}
	return newClient(c.base, cfg)
}

func newClient(base *url.URL, cfg config) (*Client, error) {
	if err := errors.Join(cfg.errs...); err != nil {
		return nil, err
	}
	tel, err := newTelemetry(cfg.meterProvider, cfg.name)
	if err != nil {
		return nil, err
	}
	if cfg.transport == nil {
		cfg.transport = defaultTransport()
	}
	transport := cfg.pipeline(tel)
	return &Client{
		base:             base,
		client:           &http.Client{Transport: transport, Timeout: cfg.timeout},
		maxResponseBytes: cfg.maxResponseBytes,
		cfg:              cfg,
	}, nil
}

func parseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("hermes: parse base URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("hermes: base URL %q must be an absolute http or https URL", u.Redacted())
	}
	u.Fragment, u.RawFragment = "", ""
	return u, nil
}

// pipeline stacks the resilience layers from the base transport outwards; the total timeout is
// not a layer but http.Client.Timeout, so it bounds redirects and the body read too.
func (c *config) pipeline(tel *telemetry) *pipelineTransport {
	logger := func() *slog.Logger { return slog.Default() }
	if l := c.logger; l != nil {
		logger = func() *slog.Logger { return l }
	}

	var rt http.RoundTripper = nonNilBodyTransport{next: c.transport}
	if len(c.headers) > 0 || c.bearerToken != nil {
		rt = &headerTransport{next: rt, headers: c.headers, token: c.bearerToken}
	}
	if c.breaker != nil {
		if c.breakers == nil {
			c.breakers = &breakerGroup{policy: *c.breaker, logger: logger, tel: tel, byHost: make(map[string]*hostBreaker)}
		}
		rt = &breakerTransport{next: rt, group: c.breakers}
	}
	if c.attemptTimeout > 0 {
		rt = &attemptTimeoutTransport{next: rt, timeout: c.attemptTimeout}
	}
	if c.limiter != nil {
		rt = &rateLimitTransport{next: rt, limiter: c.limiter}
	}
	if c.retry != nil && c.retry.MaxAttempts > 1 {
		rt = &retryTransport{next: rt, policy: *c.retry, logger: logger, tel: tel}
	}
	if c.slots != nil {
		rt = &bulkheadTransport{next: rt, slots: c.slots}
	}

	var otelOpts []otelhttp.Option
	if c.tracerProvider != nil {
		otelOpts = append(otelOpts, otelhttp.WithTracerProvider(c.tracerProvider))
	}
	if c.meterProvider != nil {
		otelOpts = append(otelOpts, otelhttp.WithMeterProvider(c.meterProvider))
	}
	if c.propagators != nil {
		otelOpts = append(otelOpts, otelhttp.WithPropagators(c.propagators))
	}
	if c.name != "" {
		otelOpts = append(otelOpts, otelhttp.WithSpanOptions(trace.WithAttributes(endpointKey.String(c.name))))
	}
	rt = otelhttp.NewTransport(rt, otelOpts...)
	if c.name != "" {
		rt = &endpointLabelTransport{next: rt, attr: endpointKey.String(c.name)}
	}
	return &pipelineTransport{RoundTripper: rt, base: c.transport}
}

// pipelineTransport forwards CloseIdleConnections to the base transport, which otelhttp's Transport does not.
type pipelineTransport struct {
	http.RoundTripper
	base http.RoundTripper
}

func (t *pipelineTransport) CloseIdleConnections() {
	if ci, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		ci.CloseIdleConnections()
	}
}

func defaultTransport() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Someone replaced the global; start from net/http's documented defaults instead.
		t = &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}
	}
	t = t.Clone()
	t.MaxIdleConnsPerHost = 100
	t.IdleConnTimeout = 90 * time.Second
	t.TLSHandshakeTimeout = 10 * time.Second
	return t
}

// NewRequest builds a request for path, a relative reference appended to the base URL path; the base and path queries are both kept.
func (c *Client) NewRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	u, err := c.resolve(path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("hermes: new request: %w", err)
	}
	return req, nil
}

func (c *Client) resolve(path string) (*url.URL, error) {
	ref, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("hermes: parse path: %w", err)
	}
	if ref.Scheme != "" || ref.Host != "" {
		return nil, fmt.Errorf("hermes: path %q must be relative to the base URL", path)
	}
	u := *c.base
	if p := ref.EscapedPath(); p != "" {
		joined := strings.TrimSuffix(c.base.EscapedPath(), "/") + "/" + strings.TrimPrefix(p, "/")
		if u.Path, err = url.PathUnescape(joined); err != nil {
			return nil, fmt.Errorf("hermes: join path: %w", err)
		}
		u.RawPath = joined
	}
	switch {
	case u.RawQuery == "":
		u.RawQuery = ref.RawQuery
	case ref.RawQuery != "":
		u.RawQuery += "&" + ref.RawQuery
	}
	return &u, nil
}

// Do sends req through the resilience pipeline; like net/http, a non-2xx status is not an error and the caller must close the body, and a transport error is a *url.Error, here without userinfo or query in its URL.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.client.Do(req)
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		urlErr.URL = redactRawURL(urlErr.URL)
	}
	return resp, err
}

// HTTPClient returns a new *http.Client backed by the same pipeline and total timeout, for generated SDKs that accept one.
func (c *Client) HTTPClient() *http.Client {
	return &http.Client{Transport: c.client.Transport, Timeout: c.client.Timeout}
}

// CloseIdleConnections closes the idle connections of the base transport, shared with the Client's endpoints, when it supports that.
func (c *Client) CloseIdleConnections() {
	c.client.CloseIdleConnections()
}
