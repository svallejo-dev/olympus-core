package hermes

import (
	"context"
	"net/http"
	"slices"
)

type headerTransport struct {
	next    http.RoundTripper
	headers http.Header
	token   func(context.Context) (string, error)
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	if out.Header == nil {
		out.Header = make(http.Header)
	}
	for k, vs := range t.headers {
		if _, ok := out.Header[k]; !ok {
			out.Header[k] = slices.Clone(vs)
		}
	}
	if t.token != nil && out.Header.Get("Authorization") == "" {
		token, err := t.token(req.Context())
		if err != nil {
			closeRequestBody(req)
			return nil, &tokenError{err: err}
		}
		out.Header.Set("Authorization", "Bearer "+token)
	}
	return t.next.RoundTrip(out)
}

// tokenError marks a failure of the bearer token function: the request never left the process,
// so it neither counts against the host's breaker nor is retried.
type tokenError struct {
	err error
}

func (e *tokenError) Error() string { return "hermes: bearer token: " + e.err.Error() }

func (e *tokenError) Unwrap() error { return e.err }
