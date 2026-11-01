package hermes_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/svallejo-dev/olympus/hermes"
)

func ExampleGetJSON() {
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
	u, err := hermes.GetJSON[user](context.Background(), c, "/users/42")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(u.ID, u.Name)

	_, err = hermes.GetJSON[user](context.Background(), c, "/users/7")
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
