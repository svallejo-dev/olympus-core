package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// maxErrorBodyBytes caps StatusError.Body.
const maxErrorBodyBytes = 4 << 10

// RequestOption adjusts a request built by Send and its shorthands just before it is sent.
type RequestOption func(*http.Request)

// Header sets a request header, overriding the Client's defaults and Send's Accept and Content-Type.
func Header(key, value string) RequestOption {
	return func(r *http.Request) { r.Header.Set(key, value) }
}

// IdempotencyKey sets the Idempotency-Key header, which makes a POST or PATCH retryable unless RetryPolicy.IdempotencyHeader names another.
func IdempotencyKey(key string) RequestOption {
	return Header(defaultIdempotencyHeader, key)
}

type idempotentKey struct{}

// Idempotent marks this request as safe to retry, for an API that deduplicates by a key in the body, the query or nothing at all.
func Idempotent() RequestOption {
	return func(r *http.Request) {
		*r = *r.WithContext(context.WithValue(r.Context(), idempotentKey{}, true))
	}
}

func markedIdempotent(ctx context.Context) bool {
	return ctx.Value(idempotentKey{}) != nil
}

// Query adds values to the request URL query, keeping the parameters already there.
func Query(values url.Values) RequestOption {
	return func(r *http.Request) {
		q := r.URL.Query()
		for k, vs := range values {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		r.URL.RawQuery = q.Encode()
	}
}

// StatusError is returned by Send, its shorthands and Decode for a non-2xx response.
type StatusError struct {
	// Method is the request method.
	Method string
	// URL is the request URL without userinfo or query, safe to log.
	URL string
	// StatusCode is the response status code.
	StatusCode int
	// Header holds the response headers, such as Retry-After or a request ID.
	Header http.Header
	// Body holds at most the first 4 KiB of the response body.
	Body []byte
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("hermes: %s %s: %d %s", e.Method, e.URL, e.StatusCode, http.StatusText(e.StatusCode))
}

// Send sends in as JSON (nil sends no body) and decodes the response as Decode does.
func Send[T any](ctx context.Context, c *Client, method, path string, in any, opts ...RequestOption) (T, error) {
	var zero T
	body, err := encodeJSON(in)
	if err != nil {
		return zero, err
	}
	req, err := c.NewRequest(ctx, method, path, body)
	if err != nil {
		return zero, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, opt := range opts {
		opt(req)
	}

	resp, err := c.Do(req) //nolint:bodyclose // decode closes it
	if err != nil {
		return zero, err
	}
	return decode[T](c, req, resp)
}

// Get is Send with GET and no request body.
func Get[T any](ctx context.Context, c *Client, path string, opts ...RequestOption) (T, error) {
	return Send[T](ctx, c, http.MethodGet, path, nil, opts...)
}

// Post is Send with POST; it is retried only when known to be idempotent, see Idempotent and RetryPolicy.
func Post[T any](ctx context.Context, c *Client, path string, in any, opts ...RequestOption) (T, error) {
	return Send[T](ctx, c, http.MethodPost, path, in, opts...)
}

// Put is Send with PUT.
func Put[T any](ctx context.Context, c *Client, path string, in any, opts ...RequestOption) (T, error) {
	return Send[T](ctx, c, http.MethodPut, path, in, opts...)
}

// Patch is Send with PATCH; it is retried only when known to be idempotent, see Idempotent and RetryPolicy.
func Patch[T any](ctx context.Context, c *Client, path string, in any, opts ...RequestOption) (T, error) {
	return Send[T](ctx, c, http.MethodPatch, path, in, opts...)
}

// Delete is Send with DELETE and no request body.
func Delete[T any](ctx context.Context, c *Client, path string, opts ...RequestOption) (T, error) {
	return Send[T](ctx, c, http.MethodDelete, path, nil, opts...)
}

// Decode reads a JSON response from Do into T, up to the Client's size limit, and closes its body; a 204 or empty body yields the zero T, and a non-2xx yields a *StatusError.
func Decode[T any](c *Client, resp *http.Response) (T, error) {
	req := resp.Request
	if req == nil {
		// Only http.Transport fills in resp.Request, and WithTransport accepts any RoundTripper.
		req = &http.Request{URL: &url.URL{}}
	}
	return decode[T](c, req, resp)
}

func decode[T any](c *Client, req *http.Request, resp *http.Response) (T, error) {
	var zero T
	defer drainAndClose(resp)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return zero, &StatusError{Method: req.Method, URL: redactURL(req.URL), StatusCode: resp.StatusCode, Header: resp.Header, Body: errBody}
	}
	if resp.StatusCode == http.StatusNoContent {
		return zero, nil
	}
	limited := &io.LimitedReader{R: resp.Body, N: c.maxResponseBytes + 1}
	var out T
	err := json.NewDecoder(limited).Decode(&out)
	switch {
	case limited.N <= 0:
		return zero, fmt.Errorf("hermes: %s %s: response body exceeds %d bytes", req.Method, redactURL(req.URL), c.maxResponseBytes)
	case errors.Is(err, io.EOF):
		return zero, nil
	case err != nil:
		return zero, fmt.Errorf("hermes: decode %s %s response: %w", req.Method, redactURL(req.URL), err)
	}
	return out, nil
}

// encodeJSON returns a *bytes.Reader so http.NewRequest sets GetBody and retries can replay the body.
func encodeJSON(in any) (io.Reader, error) {
	if in == nil {
		return nil, nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("hermes: encode request body: %w", err)
	}
	return bytes.NewReader(b), nil
}
