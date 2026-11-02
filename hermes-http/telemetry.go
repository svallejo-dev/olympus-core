package hermes

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

const instrumentationName = "github.com/svallejo-dev/olympus-core/hermes-http"

const (
	retryReasonKey = attribute.Key("hermes.retry.reason")
	endpointKey    = attribute.Key("hermes.endpoint")
)

type telemetry struct {
	endpoint     string
	retries      metric.Int64Counter
	rejections   metric.Int64Counter
	breakerState metric.Int64Gauge
}

func newTelemetry(mp metric.MeterProvider, endpoint string) (*telemetry, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(instrumentationName)
	retries, err := meter.Int64Counter("hermes.client.retries",
		metric.WithUnit("{retry}"),
		metric.WithDescription("Attempts resent after a retryable failure."))
	if err != nil {
		return nil, fmt.Errorf("hermes: create retries counter: %w", err)
	}
	rejections, err := meter.Int64Counter("hermes.client.circuit_breaker.rejections",
		metric.WithUnit("{request}"),
		metric.WithDescription("Requests rejected without being sent because the host's circuit breaker was open."))
	if err != nil {
		return nil, fmt.Errorf("hermes: create rejections counter: %w", err)
	}
	// A synchronous gauge recorded on each transition: an observable one would need a callback
	// registration that outlives the Client, which has no Close to unregister it.
	state, err := meter.Int64Gauge("hermes.client.circuit_breaker.state",
		metric.WithUnit("{state}"),
		metric.WithDescription("Circuit breaker state per host: 0 closed, 1 half-open, 2 open."))
	if err != nil {
		return nil, fmt.Errorf("hermes: create breaker state gauge: %w", err)
	}
	return &telemetry{endpoint: endpoint, retries: retries, rejections: rejections, breakerState: state}, nil
}

// attributes identify a host and, on an Endpoint, which of the Client's breakers and policies acted on it.
func (t *telemetry) attributes(u *url.URL) []attribute.KeyValue {
	attrs := []attribute.KeyValue{semconv.ServerAddress(u.Hostname()), semconv.ServerPort(port(u))}
	if t.endpoint != "" {
		attrs = append(attrs, endpointKey.String(t.endpoint))
	}
	return attrs
}

func (t *telemetry) breakerName(key string) string {
	if t.endpoint == "" {
		return key
	}
	return t.endpoint + " " + key
}

// endpointLabelTransport adds the endpoint to the http.client.* metrics, which otelhttp reads from a labeler in the context.
type endpointLabelTransport struct {
	next http.RoundTripper
	attr attribute.KeyValue
}

func (t *endpointLabelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A labeler of its own: otelhttp shares the one in the context with nested calls, such as the bearer token's.
	labeler := &otelhttp.Labeler{}
	if outer, ok := otelhttp.ClientLabelerFromContext(req.Context()); ok {
		labeler.Add(outer.Get()...)
	}
	labeler.Add(t.attr)
	return t.next.RoundTrip(req.WithContext(otelhttp.ContextWithClientLabeler(req.Context(), labeler)))
}

// port returns the URL's port, or the scheme's default when it has none.
func port(u *url.URL) int {
	if p, err := strconv.Atoi(u.Port()); err == nil {
		return p
	}
	if u.Scheme == "https" {
		return 443
	}
	return 80
}

// redactURL drops userinfo and query, which may carry credentials, for logs and errors.
func redactURL(u *url.URL) string {
	r := *u
	r.User, r.RawQuery, r.ForceQuery, r.Fragment, r.RawFragment = nil, "", false, "", ""
	return r.String()
}

// redactRawURL is redactURL for a URL string such as url.Error.URL; a string that does not parse is dropped whole.
func redactRawURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[redacted]"
	}
	return redactURL(u)
}
