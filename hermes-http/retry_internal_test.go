package hermes

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in     string
		want   time.Duration
		wantOK bool
	}{
		{"", 0, false},
		{"3", 3 * time.Second, true},
		{"-1", 0, false},
		{"soon", 0, false},
		{now.Add(5 * time.Second).Format(http.TimeFormat), 5 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
	}
	for _, tt := range tests {
		got, ok := parseRetryAfter(tt.in, now)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("parseRetryAfter(%q) = %v, %v; want %v, %v", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestBackoffStaysWithinExponentialCeiling(t *testing.T) {
	p := RetryPolicy{}.withDefaults()
	for attempt, ceiling := range map[int]time.Duration{
		1:  100 * time.Millisecond,
		2:  200 * time.Millisecond,
		5:  1600 * time.Millisecond,
		6:  2 * time.Second,
		90: 2 * time.Second,
	} {
		for range 100 {
			if d, ok := p.backoff(attempt, nil); !ok || d < 0 || d > ceiling {
				t.Fatalf("backoff(%d) = %v, want within [0, %v]", attempt, d, ceiling)
			}
		}
	}
}

func TestIdempotent(t *testing.T) {
	for method, want := range map[string]bool{
		http.MethodGet: true, http.MethodHead: true, http.MethodOptions: true, http.MethodPut: true,
		http.MethodDelete: true, http.MethodTrace: true, http.MethodPost: false, http.MethodPatch: false,
	} {
		req, err := http.NewRequestWithContext(t.Context(), method, "https://api.test", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := idempotent(req); got != want {
			t.Errorf("idempotent(%s) = %v, want %v", method, got, want)
		}
		req.Header.Set("Idempotency-Key", "k")
		if !idempotent(req) {
			t.Errorf("idempotent(%s with Idempotency-Key) = false, want true", method)
		}
	}
}
