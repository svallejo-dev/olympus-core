package hermes_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	"github.com/svallejo-dev/olympus-core/hermes-http"
)

func ExampleGet() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/users/42" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":42,"name":"Ada"}`)
	}))
	defer srv.Close()

	c, err := hermes.New(srv.URL+"/v1", hermes.WithUserAgent("billing/1.0"))
	if err != nil {
		log.Fatal(err)
	}

	type user struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	u, err := hermes.Get[user](context.Background(), c, "/users/42")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(u.ID, u.Name)

	_, err = hermes.Get[user](context.Background(), c, "/users/7")
	if statusErr, ok := errors.AsType[*hermes.StatusError](err); ok {
		fmt.Println("status", statusErr.StatusCode)
	}
	// Output:
	// 42 Ada
	// status 404
}

func ExampleClient_HTTPClient() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, err := hermes.New(srv.URL,
		hermes.WithTimeout(5*time.Second),
		hermes.WithRetry(hermes.RetryPolicy{MaxAttempts: 4, InitialBackoff: 50 * time.Millisecond}),
		hermes.WithCircuitBreaker(hermes.BreakerPolicy{ConsecutiveFailures: 10, OpenTimeout: time.Minute}),
		hermes.WithMaxConcurrent(32),
	)
	if err != nil {
		log.Fatal(err)
	}

	// Hand the pipeline to a generated SDK, e.g. oapi-codegen's WithHTTPClient(c.HTTPClient()).
	httpClient := c.HTTPClient()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/health", nil)
	if err != nil {
		log.Fatal(err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	fmt.Println(resp.StatusCode)
	// Output: 204
}

func ExampleDecode() {
	c, err := hermes.New("https://api.example.com")
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()

	req, err := c.NewRequest(ctx, http.MethodGet, "/users", nil)
	if err != nil {
		log.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	users, err := hermes.Decode[[]string](c, resp)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(users, resp.Header.Get("Link"))
}

func ExampleClient_Endpoint() {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c, err := hermes.New(srv.URL, hermes.WithoutCircuitBreaker(), hermes.WithRetry(hermes.RetryPolicy{
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
	}))
	if err != nil {
		log.Fatal(err)
	}
	reports, err := c.Endpoint("reports", hermes.WithoutRetry())
	if err != nil {
		log.Fatal(err)
	}

	type report struct{}
	_, _ = hermes.Get[report](context.Background(), reports, "/reports/1")
	fmt.Println("reports:", hits.Swap(0))

	_, _ = hermes.Get[report](context.Background(), c, "/orders/1")
	fmt.Println("orders:", hits.Load())
	// Output:
	// reports: 1
	// orders: 3
}
