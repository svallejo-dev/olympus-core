package atlas_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/svallejo-dev/olympus/atlas"
)

// rawGet sends target as is, so neither the client nor the test can clean the path first.
func rawGet(t *testing.T, url, target string) response {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: atlas\r\nConnection: close\r\n\r\n", target); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response to %s: %v", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of %s: %v", target, err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: string(body)}
}

func echoPath(prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, prefix+" "+r.Method+" "+r.URL.Path)
	})
}

func TestAppSeesRawPaths(t *testing.T) {
	t.Parallel()
	modes := map[string][]atlas.Option{
		"shared": nil,
		"admin":  {atlas.WithAdminListener(listenLocal(t))},
	}
	for mode, opts := range modes {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			svc := start(t, echoPath("app"), opts...)
			for _, target := range []string{"//a//b", "/a/../b", "/a/./b", "//livez"} {
				got := rawGet(t, svc.url, target)
				if want := "app GET " + target; got.status != http.StatusOK || got.body != want {
					t.Errorf("GET %s = %d %q, want 200 %q", target, got.status, got.body, want)
				}
			}
		})
	}
}

func TestOpsHandler(t *testing.T) {
	t.Parallel()
	opts := []atlas.Option{
		atlas.WithOpsHandler("/debug/vars", echoPath("vars")),
		atlas.WithOpsHandler("/debug/pprof/", echoPath("pprof")),
		atlas.WithOpsHandler("/debug/pprof/cmdline/", echoPath("cmdline")),
	}

	t.Run("shared", func(t *testing.T) {
		t.Parallel()
		svc := start(t, echoPath("app"), opts...)
		for target, want := range map[string]string{
			"/debug/vars":                "vars GET /debug/vars",
			"/debug/vars/x":              "app GET /debug/vars/x",
			"/debug/pprof/":              "pprof GET /debug/pprof/",
			"/debug/pprof/heap":          "pprof GET /debug/pprof/heap",
			"/debug/pprof/cmdline/extra": "cmdline GET /debug/pprof/cmdline/extra",
			"/debug/pprof":               "app GET /debug/pprof",
			"/livez/":                    "app GET /livez/",
		} {
			if got := rawGet(t, svc.url, target); got.body != want {
				t.Errorf("GET %s = %d %q, want %q", target, got.status, got.body, want)
			}
		}
		resp, err := http.Post(svc.url+"/debug/pprof/symbol", "text/plain", nil)
		if err != nil {
			t.Fatalf("POST /debug/pprof/symbol: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if want := "pprof POST /debug/pprof/symbol"; string(body) != want {
			t.Errorf("POST /debug/pprof/symbol = %q, want %q", body, want)
		}
	})

	t.Run("admin", func(t *testing.T) {
		t.Parallel()
		adminLn := listenLocal(t)
		adminURL := "http://" + adminLn.Addr().String()
		svc := start(t, echoPath("app"), append(opts, atlas.WithAdminListener(adminLn))...)
		if got := get(t, adminURL+"/debug/pprof/heap"); got.body != "pprof GET /debug/pprof/heap" {
			t.Errorf("admin GET /debug/pprof/heap = %d %q, want the ops handler", got.status, got.body)
		}
		if got := get(t, svc.url+"/debug/pprof/heap"); got.body != "app GET /debug/pprof/heap" {
			t.Errorf("app GET /debug/pprof/heap = %d %q, want the app", got.status, got.body)
		}
		if got := get(t, adminURL+"/elsewhere"); got.status != http.StatusNotFound {
			t.Errorf("admin GET /elsewhere = %d, want 404", got.status)
		}
	})
}

func TestPrometheusGathererJoinsMetrics(t *testing.T) {
	t.Parallel()
	// Like prometheus.DefaultGatherer: its own Go collector plus an application metric.
	registry := prometheus.NewRegistry()
	legacy := prometheus.NewCounter(prometheus.CounterOpts{Name: "orders_legacy_total", Help: "Legacy orders."})
	registry.MustRegister(collectors.NewGoCollector(), legacy)
	legacy.Add(3)
	svc := start(t, helloMux(), atlas.WithPrometheusGatherer(registry))

	get(t, svc.url+"/hello")
	got := get(t, svc.url+"/metrics")
	if got.status != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200:\n%s", got.status, got.body)
	}
	for _, want := range []string{"orders_legacy_total 3", "http_server_request_duration_seconds_count{"} {
		if !strings.Contains(got.body, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
	if n := strings.Count(got.body, "# TYPE go_goroutines "); n != 1 {
		t.Errorf("/metrics has go_goroutines %d times, want 1", n)
	}
}
