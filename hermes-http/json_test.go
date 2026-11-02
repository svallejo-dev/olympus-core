package hermes_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/svallejo-dev/olympus-core/hermes-http"
)

type order struct {
	ID    string `json:"id"`
	Total int    `json:"total"`
}

func TestPostRoundTrip(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var in order
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("decode request: %v", err)
		}
		in.ID = "ord-1"
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(in)
	})
	c := newClient(t, srv.URL)

	got, err := hermes.Post[order](t.Context(), c, "/orders", order{Total: 42})
	if err != nil {
		t.Fatal(err)
	}
	if want := (order{ID: "ord-1", Total: 42}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestSendEmptyResponses(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"no content", http.StatusNoContent},
		{"empty body", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tt.status) })
			c := newClient(t, srv.URL)
			got, err := hermes.Send[order](t.Context(), c, http.MethodDelete, "/orders/1", nil)
			if err != nil || got != (order{}) {
				t.Errorf("got %+v, %v; want the zero value and no error", got, err)
			}
		})
	}
}

func TestStatusError(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "req-1")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(strings.Repeat("x", 10_000)))
	})
	base := strings.Replace(srv.URL, "http://", "http://user:secret@", 1)
	c := newClient(t, base)

	_, err := hermes.Get[order](t.Context(), c, "/orders?token=s3cr3t")
	statusErr, ok := errors.AsType[*hermes.StatusError](err)
	if !ok {
		t.Fatalf("err = %v, want *StatusError", err)
	}
	if statusErr.Method != http.MethodGet || statusErr.StatusCode != http.StatusTeapot || statusErr.URL != srv.URL+"/orders" {
		t.Errorf("StatusError = {%s %s %d}, want {GET %s/orders 418}", statusErr.Method, statusErr.URL, statusErr.StatusCode, srv.URL)
	}
	if got := statusErr.Header.Get("X-Request-Id"); got != "req-1" {
		t.Errorf("Header X-Request-Id = %q, want req-1", got)
	}
	if len(statusErr.Body) != 4096 {
		t.Errorf("len(Body) = %d, want 4096", len(statusErr.Body))
	}
	msg := err.Error()
	for _, leak := range []string{"secret", "s3cr3t", "token"} {
		if strings.Contains(msg, leak) {
			t.Errorf("Error() = %q leaks %q", msg, leak)
		}
	}
	if want := "hermes: GET " + srv.URL + "/orders: 418 I'm a teapot"; msg != want {
		t.Errorf("Error() = %q, want %q", msg, want)
	}
}

func TestSendResponseTooLarge(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + strings.Repeat("a", 100) + `"}`))
	})
	c := newClient(t, srv.URL, hermes.WithMaxResponseBytes(32))

	if _, err := hermes.Get[order](t.Context(), c, "/"); err == nil || !strings.Contains(err.Error(), "exceeds 32 bytes") {
		t.Errorf("err = %v, want a size limit error", err)
	}
}

func TestQueryOptionKeepsExistingParameters(t *testing.T) {
	got := make(chan url.Values, 1)
	srv := newServer(t, func(_ http.ResponseWriter, r *http.Request) { got <- r.URL.Query() })
	c := newClient(t, srv.URL)

	if _, err := hermes.Get[struct{}](t.Context(), c, "/orders?status=open", hermes.Query(url.Values{"page": {"2"}})); err != nil {
		t.Fatal(err)
	}
	if q := <-got; q.Get("status") != "open" || q.Get("page") != "2" {
		t.Errorf("query = %v, want status=open and page=2", q)
	}
}

func TestVerbShorthands(t *testing.T) {
	var method, body string
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, body = r.Method, string(b)
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	c := newClient(t, srv.URL)
	type result struct {
		OK bool `json:"ok"`
	}
	in := map[string]int{"n": 1}
	const sent = `{"n":1}`

	tests := []struct {
		wantMethod, wantBody string
		call                 func() (result, error)
	}{
		{http.MethodGet, "", func() (result, error) { return hermes.Get[result](t.Context(), c, "/") }},
		{http.MethodPost, sent, func() (result, error) { return hermes.Post[result](t.Context(), c, "/", in) }},
		{http.MethodPut, sent, func() (result, error) { return hermes.Put[result](t.Context(), c, "/", in) }},
		{http.MethodPatch, sent, func() (result, error) { return hermes.Patch[result](t.Context(), c, "/", in) }},
		{http.MethodDelete, "", func() (result, error) { return hermes.Delete[result](t.Context(), c, "/") }},
		{"OPTIONS", sent, func() (result, error) { return hermes.Send[result](t.Context(), c, "OPTIONS", "/", in) }},
	}
	for _, tt := range tests {
		got, err := tt.call()
		if err != nil || !got.OK {
			t.Errorf("%s: got %+v, %v", tt.wantMethod, got, err)
		}
		if method != tt.wantMethod || body != tt.wantBody {
			t.Errorf("server saw %s %q, want %s %q", method, body, tt.wantMethod, tt.wantBody)
		}
	}
}

func TestDecodeKeepsTheResponseHeaders(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `</users?page=2>; rel="next"`)
		_, _ = io.WriteString(w, `[{"id":1}]`)
	})
	c := newClient(t, srv.URL)
	req, err := c.NewRequest(t.Context(), http.MethodGet, "/users", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	users, err := hermes.Decode[[]struct {
		ID int `json:"id"`
	}](c, resp)
	if err != nil || len(users) != 1 || users[0].ID != 1 {
		t.Errorf("Decode = %+v, %v", users, err)
	}
	if !strings.Contains(resp.Header.Get("Link"), "page=2") {
		t.Errorf("Link = %q", resp.Header.Get("Link"))
	}
}

func TestDecodeReportsStatusErrors(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, "already exists")
	})
	c := newClient(t, srv.URL)
	req, err := c.NewRequest(t.Context(), http.MethodDelete, "/users/1?token=secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_, err = hermes.Decode[struct{}](c, resp)
	statusErr, ok := errors.AsType[*hermes.StatusError](err)
	if !ok {
		t.Fatalf("Decode error = %v, want a *StatusError", err)
	}
	if statusErr.StatusCode != http.StatusConflict || statusErr.Method != http.MethodDelete || string(statusErr.Body) != "already exists" {
		t.Errorf("StatusError = %+v", statusErr)
	}
	if strings.Contains(statusErr.URL, "secret") || !strings.Contains(statusErr.URL, "/users/1") {
		t.Errorf("URL = %q, want the path without the query", statusErr.URL)
	}
}

func TestDecodeReleasesTheConcurrencySlot(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
	c := newClient(t, srv.URL, hermes.WithMaxConcurrent(1))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	for range 3 {
		req, err := c.NewRequest(ctx, http.MethodGet, "/", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("Do blocked on a slot Decode should have released: %v", err)
		}
		if _, err := hermes.Decode[struct{}](c, resp); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDecodeRespectsTheSizeLimit(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `"`+strings.Repeat("x", 100)+`"`)
	})
	c := newClient(t, srv.URL, hermes.WithMaxResponseBytes(10))
	req, err := c.NewRequest(t.Context(), http.MethodGet, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hermes.Decode[string](c, resp); err == nil || !strings.Contains(err.Error(), "exceeds 10 bytes") {
		t.Errorf("Decode error = %v, want the size limit", err)
	}
}

// bareTransport returns a response without Request, which only http.Transport fills in.
type bareTransport struct{}

func (bareTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("down"))}, nil
}

func TestDecodeWithoutARequestOnTheResponse(t *testing.T) {
	c := newClient(t, "http://api.test", hermes.WithTransport(bareTransport{}), hermes.WithoutRetry())
	req, err := c.NewRequest(t.Context(), http.MethodGet, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Request = nil
	_, err = hermes.Decode[struct{}](c, resp)
	if _, ok := errors.AsType[*hermes.StatusError](err); !ok {
		t.Errorf("Decode error = %v, want a *StatusError", err)
	}
}
