package hermes_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/svallejo-dev/olympus-core/hermes"
)

type order struct {
	ID    string `json:"id"`
	Total int    `json:"total"`
}

func TestPostJSONRoundTrip(t *testing.T) {
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

	got, err := hermes.PostJSON[order](t.Context(), c, "/orders", order{Total: 42})
	if err != nil {
		t.Fatal(err)
	}
	if want := (order{ID: "ord-1", Total: 42}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestDoJSONEmptyResponses(t *testing.T) {
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
			got, err := hermes.DoJSON[order](t.Context(), c, http.MethodDelete, "/orders/1", nil)
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

	_, err := hermes.GetJSON[order](t.Context(), c, "/orders?token=s3cr3t")
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

func TestDoJSONResponseTooLarge(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + strings.Repeat("a", 100) + `"}`))
	})
	c := newClient(t, srv.URL, hermes.WithMaxResponseBytes(32))

	if _, err := hermes.GetJSON[order](t.Context(), c, "/"); err == nil || !strings.Contains(err.Error(), "exceeds 32 bytes") {
		t.Errorf("err = %v, want a size limit error", err)
	}
}

func TestQueryOptionKeepsExistingParameters(t *testing.T) {
	got := make(chan url.Values, 1)
	srv := newServer(t, func(_ http.ResponseWriter, r *http.Request) { got <- r.URL.Query() })
	c := newClient(t, srv.URL)

	if _, err := hermes.GetJSON[struct{}](t.Context(), c, "/orders?status=open", hermes.Query(url.Values{"page": {"2"}})); err != nil {
		t.Fatal(err)
	}
	if q := <-got; q.Get("status") != "open" || q.Get("page") != "2" {
		t.Errorf("query = %v, want status=open and page=2", q)
	}
}
