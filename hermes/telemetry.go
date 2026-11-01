package hermes

import (
	"fmt"
	"net/url"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

const instrumentationName = "github.com/svallejo-dev/olympus/hermes"

const retryReasonKey = attribute.Key("hermes.retry.reason")

type telemetry struct {
	retries      metric.Int64Counter
	rejections   metric.Int64Counter
	breakerState metric.Int64Gauge
}

func newTelemetry(mp metric.MeterProvider) (*telemetry, error) {
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
	return &telemetry{retries: retries, rejections: rejections, breakerState: state}, nil
}

func serverAttributes(u *url.URL) []attribute.KeyValue {
	return []attribute.KeyValue{semconv.ServerAddress(u.Hostname()), semconv.ServerPort(port(u))}
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
