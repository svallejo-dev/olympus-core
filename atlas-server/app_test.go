package atlas_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/svallejo-dev/olympus-core/atlas-server"
)

type problemBody struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	RequestID string `json:"request_id"`
}

func serveApp(app http.Handler, method, target string, body io.Reader) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(method, target, body))
	return rec
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) problemBody {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json; body %s", got, rec.Body)
	}
	var p problemBody
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem %q: %v", rec.Body, err)
	}
	if p.Type != "about:blank" || p.Status != rec.Code || p.Title != http.StatusText(rec.Code) {
		t.Errorf("problem = %+v for status %d", p, rec.Code)
	}
	return p
}

func text(s string) atlas.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) error {
		_, err := io.WriteString(w, s)
		return err
	}
}

// tag is middleware that appends name to a response header on the way in.
func tag(name string) atlas.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("X-Chain", name)
			next.ServeHTTP(w, r)
		})
	}
}

func quietApp(opts ...atlas.Option) *atlas.App {
	return atlas.New(append([]atlas.Option{atlas.WithLogger(slog.New(slog.DiscardHandler))}, opts...)...)
}

func TestRoutes(t *testing.T) {
	t.Parallel()
	app := quietApp()
	app.Get("/", text("root"))
	app.Get("/orders/{id}", func(w http.ResponseWriter, r *http.Request) error {
		return atlas.JSON(w, http.StatusOK, map[string]string{"id": r.PathValue("id")})
	})
	app.Post("/orders", text("post"))
	app.Put("/orders/{id}", text("put"))
	app.Patch("/orders/{id}", text("patch"))
	app.Delete("/orders/{id}", text("delete"))
	app.Method(http.MethodOptions, "/orders", text("options"))
	app.Get("/files/{path...}", func(w http.ResponseWriter, r *http.Request) error {
		_, err := io.WriteString(w, r.PathValue("path"))
		return err
	})
	v1 := app.Group("/v1")
	v1.Get("/", text("v1 root"))
	v1.Get("/ping-me/", text("trailing"))
	v1.Group("/admin").Get("/users", text("nested"))

	tests := []struct {
		method, target string
		wantStatus     int
		wantBody       string
	}{
		{"GET", "/", 200, "root"},
		{"POST", "/orders", 200, "post"},
		{"PUT", "/orders/1", 200, "put"},
		{"PATCH", "/orders/1", 200, "patch"},
		{"DELETE", "/orders/1", 200, "delete"},
		{"OPTIONS", "/orders", 200, "options"},
		{"GET", "/orders/42", 200, `{"id":"42"}` + "\n"},
		{"GET", "/files/a/b.txt", 200, "a/b.txt"},
		{"GET", "/v1", 200, "v1 root"},
		{"GET", "/v1/ping-me/", 200, "trailing"},
		{"GET", "/v1/admin/users", 200, "nested"},
	}
	for _, tt := range tests {
		rec := serveApp(app, tt.method, tt.target, nil)
		if rec.Code != tt.wantStatus || rec.Body.String() != tt.wantBody {
			t.Errorf("%s %s = %d %q, want %d %q", tt.method, tt.target, rec.Code, rec.Body, tt.wantStatus, tt.wantBody)
		}
	}
	// "/" and trailing slashes match exactly, unlike a bare ServeMux pattern.
	for _, target := range []string{"/nope", "/v1/", "/v1/ping-me/x"} {
		if rec := serveApp(app, "GET", target, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, rec.Code)
		}
	}
	// ServeMux redirects to the slash form of a route registered with a trailing slash.
	if rec := serveApp(app, "GET", "/v1/ping-me", nil); rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("GET /v1/ping-me = %d, want 307", rec.Code)
	}
	if rec := serveApp(app, "HEAD", "/", nil); rec.Code != http.StatusOK {
		t.Errorf("HEAD / = %d, want GET's handler", rec.Code)
	}
}

func TestMiddlewareOrderAndScope(t *testing.T) {
	t.Parallel()
	app := quietApp()
	app.Use(tag("app1"), tag("app2"))
	app.Get("/open", text("ok"))
	v1 := app.Group("/v1", tag("group"))
	v1.Use(tag("group-use"))
	v1.Get("/a", text("ok"), tag("route"))
	v1.Group("/nested", tag("nested")).Get("/b", text("ok"))
	app.Handle("GET /std", http.NotFoundHandler(), tag("std"))

	tests := map[string]string{
		"/open":        "app1,app2",
		"/v1/a":        "app1,app2,group,group-use,route",
		"/v1/nested/b": "app1,app2,group,group-use,nested",
		"/std":         "app1,app2,std",
		"/unmatched":   "app1,app2",
	}
	for target, want := range tests {
		rec := serveApp(app, "GET", target, nil)
		if got := strings.Join(rec.Header().Values("X-Chain"), ","); got != want {
			t.Errorf("GET %s chain = %q, want %q", target, got, want)
		}
	}
}

func TestAppMiddlewareSeesUnmatchedMethods(t *testing.T) {
	t.Parallel()
	app := quietApp()
	// A CORS middleware has to answer a preflight that no route matches.
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	app.Post("/orders", text("ok"))
	if rec := serveApp(app, "OPTIONS", "/orders", nil); rec.Code != http.StatusNoContent {
		t.Errorf("OPTIONS /orders = %d, want 204 from the middleware", rec.Code)
	}
}

func TestUnmatchedRequestsAreProblems(t *testing.T) {
	t.Parallel()
	app := quietApp()
	app.Get("/orders", text("ok"))
	app.Post("/orders", text("ok"))

	rec := serveApp(app, "GET", "/nope", nil)
	if p := decodeProblem(t, rec); rec.Code != http.StatusNotFound || p.Detail == "" {
		t.Errorf("GET /nope = %d %+v", rec.Code, p)
	}
	rec = serveApp(app, "DELETE", "/orders", nil)
	decodeProblem(t, rec)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD, POST" {
		t.Errorf("DELETE /orders = %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
	// The path-cleaning redirect is not an error.
	if rec = serveApp(app, "GET", "//orders", nil); rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("GET //orders = %d, want 307", rec.Code)
	}
}

// domainError shows an error type choosing its status without importing atlas.
type domainError struct{ status int }

func (e domainError) Error() string   { return "domain says no" }
func (e domainError) HTTPStatus() int { return e.status }

func TestHandlerErrors(t *testing.T) {
	t.Parallel()
	secret := errors.New("dial 10.0.3.7:5432: connection refused")
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantDetail string
		wantLogged bool
	}{
		{"Errorf", atlas.Errorf(http.StatusNotFound, "order %s not found", "42"), 404, "order 42 not found", false},
		{"wrapped Errorf", wrapErr(atlas.Errorf(http.StatusConflict, "duplicate")), 409, "duplicate", false},
		{"own type", domainError{status: http.StatusTeapot}, 418, "domain says no", false},
		{"unknown error hides its message", secret, 500, "", true},
		{"own type with a 5xx hides its message", domainError{status: http.StatusBadGateway}, 502, "", true},
		{"status below 400 is a bug", domainError{status: http.StatusOK}, 500, "", true},
		{"status above 599 is a bug", domainError{status: 700}, 500, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logs := newLogSink()
			app := atlas.New(atlas.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
			app.Get("/", func(http.ResponseWriter, *http.Request) error { return tt.err })

			rec := serveApp(app, "GET", "/", nil)
			p := decodeProblem(t, rec)
			if rec.Code != tt.wantStatus || p.Detail != tt.wantDetail {
				t.Errorf("response = %d %+v, want %d detail %q", rec.Code, p, tt.wantStatus, tt.wantDetail)
			}
			if strings.Contains(rec.Body.String(), "10.0.3.7") {
				t.Errorf("response leaks the error: %s", rec.Body)
			}
			if logged := strings.Contains(logs.String(), "request failed"); logged != tt.wantLogged {
				t.Errorf("logged = %v, want %v:\n%s", logged, tt.wantLogged, logs)
			}
			if tt.wantLogged && !strings.Contains(logs.String(), tt.err.Error()) {
				t.Errorf("log lacks the error:\n%s", logs)
			}
		})
	}
}

func wrapErr(err error) error { return errors.Join(errors.New("context"), err) }

// sdkError looks like an AWS or similar SDK error: its status belongs to another service.
type sdkError struct{}

func (sdkError) Error() string   { return "AccessDenied: arn:aws:iam::123456789:role/secret" }
func (sdkError) StatusCode() int { return http.StatusForbidden }

func TestSDKErrorStatusIsNotPropagated(t *testing.T) {
	t.Parallel()
	app := quietApp()
	app.Get("/", func(http.ResponseWriter, *http.Request) error { return wrapErr(sdkError{}) })

	rec := serveApp(app, "GET", "/", nil)
	p := decodeProblem(t, rec)
	if rec.Code != http.StatusInternalServerError || p.Detail != "" || strings.Contains(rec.Body.String(), "arn:aws") {
		t.Errorf("response = %d %s, want a 500 without details", rec.Code, rec.Body)
	}
}

func TestStatusOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		want int
	}{
		{errors.New("x"), 500},
		{atlas.Errorf(http.StatusNotFound, "x"), 404},
		{wrapErr(domainError{http.StatusTeapot}), 418},
		{domainError{http.StatusOK}, 500},
	}
	for _, tt := range tests {
		if got := atlas.StatusOf(tt.err); got != tt.want {
			t.Errorf("StatusOf(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}

func TestErrorDropsHeadersOfTheResponseItReplaces(t *testing.T) {
	t.Parallel()
	dropped := []string{"Cache-Control", "Content-Encoding", "ETag", "Last-Modified", "Content-Length"}
	setHeaders := func(w http.ResponseWriter) {
		for _, name := range dropped {
			w.Header().Set(name, "1")
		}
		w.Header().Set("X-Kept", "yes")
	}
	app := quietApp()
	app.Get("/fail", func(w http.ResponseWriter, _ *http.Request) error {
		setHeaders(w)
		return errors.New("boom")
	})
	app.Get("/panic", func(w http.ResponseWriter, _ *http.Request) error {
		setHeaders(w)
		panic("boom")
	})
	for _, target := range []string{"/fail", "/panic"} {
		rec := serveApp(app, "GET", target, nil)
		for _, name := range dropped {
			if got := rec.Header().Get(name); got != "" {
				t.Errorf("GET %s kept %s: %q", target, name, got)
			}
		}
		if rec.Header().Get("X-Kept") != "yes" {
			t.Errorf("GET %s lost an unrelated header", target)
		}
	}
}

func TestProblemTitleForAnUnknownStatus(t *testing.T) {
	t.Parallel()
	app := quietApp()
	app.Get("/", func(http.ResponseWriter, *http.Request) error { return atlas.Errorf(420, "calm down") })
	rec := serveApp(app, "GET", "/", nil)
	var p problemBody
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p.Title != "Error" || p.Status != 420 {
		t.Errorf("problem = %+v, %v", p, err)
	}
}

func TestSwitchingProtocolsThenErrorDoesNotOverwriteTheStatus(t *testing.T) {
	t.Parallel()
	app := quietApp()
	app.Get("/", func(w http.ResponseWriter, _ *http.Request) error {
		w.WriteHeader(http.StatusSwitchingProtocols)
		return errors.New("upgrade failed")
	})
	defer func() {
		if v := recover(); v != http.ErrAbortHandler { //nolint:errorlint // recover returns the exact sentinel
			t.Errorf("recover() = %v, want http.ErrAbortHandler", v)
		}
	}()
	serveApp(app, "GET", "/", nil)
}

func TestMethodIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	app := quietApp()
	app.Method("get", "/x", text("ok"))
	if rec := serveApp(app, "GET", "/x", nil); rec.Body.String() != "ok" {
		t.Errorf("GET /x = %d %q", rec.Code, rec.Body)
	}
}

func TestHandleKeepsServeMuxMeaningOfTrailingSlash(t *testing.T) {
	t.Parallel()
	app := quietApp()
	app.Handle("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.URL.Path) }))
	if rec := serveApp(app, "GET", "/static/a.css", nil); rec.Body.String() != "/static/a.css" {
		t.Errorf("GET /static/a.css = %d %q", rec.Code, rec.Body)
	}
}

func TestMountWithEncodedPrefixStillMatches(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.URL.Path+"|"+r.URL.RawPath) })
	app := quietApp()
	app.Mount("/admin", inner)
	for target, want := range map[string]string{
		"/%61dmin/x":   "/x|",
		"/admin/a%2Fb": "/a/b|/a%2Fb",
		"/admin?q=1":   "/|",
	} {
		if rec := serveApp(app, "GET", target, nil); rec.Body.String() != want {
			t.Errorf("GET %s = %d %q, want %q", target, rec.Code, rec.Body, want)
		}
	}
}

func TestHandlerErrorAfterResponseStarted(t *testing.T) {
	t.Parallel()
	logs := newLogSink()
	app := atlas.New(atlas.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	app.Get("/", func(w http.ResponseWriter, _ *http.Request) error {
		_, _ = io.WriteString(w, "partial")
		return errors.New("stream broke")
	})

	defer func() {
		if v := recover(); v != http.ErrAbortHandler { //nolint:errorlint // recover returns the exact sentinel
			t.Errorf("recover() = %v, want http.ErrAbortHandler", v)
		}
		if !strings.Contains(logs.String(), "stream broke") {
			t.Errorf("log lacks the error:\n%s", logs)
		}
	}()
	serveApp(app, "GET", "/", nil)
}

func TestCanceledRequestIsNotAServerError(t *testing.T) {
	t.Parallel()
	logs := newLogSink()
	app := atlas.New(atlas.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	app.Get("/", func(_ http.ResponseWriter, r *http.Request) error { return r.Context().Err() })

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, "GET", "/", nil))
	if rec.Code != 499 || logs.String() != "" {
		t.Errorf("status = %d, logs:\n%s\nwant 499 and no logs", rec.Code, logs)
	}
}

func TestWithErrorHandler(t *testing.T) {
	t.Parallel()
	app := quietApp(atlas.WithErrorHandler(func(w http.ResponseWriter, _ *http.Request, err error) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "custom: "+err.Error())
	}))
	app.Get("/fail", func(http.ResponseWriter, *http.Request) error { return errors.New("boom") })
	app.Get("/panic", func(http.ResponseWriter, *http.Request) error { panic("boom") })

	for target, want := range map[string]string{
		"/fail":  "custom: boom",
		"/panic": "custom: internal server error",
		"/nope":  "custom: no route matches this path",
	} {
		rec := serveApp(app, "GET", target, nil)
		if rec.Code != http.StatusTeapot || rec.Body.String() != want {
			t.Errorf("GET %s = %d %q, want 418 %q", target, rec.Code, rec.Body, want)
		}
	}
}

func TestPanicBecomesProblem(t *testing.T) {
	t.Parallel()
	logs := newLogSink()
	app := atlas.New(atlas.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	app.Get("/", func(http.ResponseWriter, *http.Request) error { panic("secret detail") })

	rec := serveApp(app, "GET", "/", nil)
	decodeProblem(t, rec)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("response = %d %s", rec.Code, rec.Body)
	}
	if len(logs.records(t, "panic recovered")) != 1 {
		t.Errorf("missing panic log:\n%s", logs)
	}
}

func TestHandleAndMount(t *testing.T) {
	t.Parallel()
	nested := http.NewServeMux()
	nested.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "nested root") })
	nested.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.URL.Path) })

	app := quietApp()
	app.Handle("GET /std", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "std") }))
	app.Handle("/any", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.Method) }))
	app.Group("/api").Mount("/legacy", nested, tag("mounted"))

	tests := []struct{ method, target, want string }{
		{"GET", "/std", "std"},
		{"POST", "/any", "POST"},
		{"GET", "/api/legacy", "nested root"},
		{"GET", "/api/legacy/", "nested root"},
		{"GET", "/api/legacy/users/7", "/users/7"},
	}
	for _, tt := range tests {
		rec := serveApp(app, tt.method, tt.target, nil)
		if rec.Code != http.StatusOK || rec.Body.String() != tt.want {
			t.Errorf("%s %s = %d %q, want %q", tt.method, tt.target, rec.Code, rec.Body, tt.want)
		}
	}
	if rec := serveApp(app, "GET", "/api/legacy", nil); rec.Header().Get("X-Chain") != "mounted" {
		t.Errorf("mount middleware did not run: %v", rec.Header())
	}
}

func TestRegistrationPanics(t *testing.T) {
	t.Parallel()
	ok := text("ok")
	std := http.NotFoundHandler()
	tests := map[string]func(*atlas.App){
		"Use after a route": func(a *atlas.App) { a.Get("/x", ok); a.Use(tag("late")) },
		"Use after a group": func(a *atlas.App) { a.Group("/x"); a.Use(tag("late")) },
		"group Use after a route": func(a *atlas.App) {
			g := a.Group("/x")
			g.Get("/y", ok)
			g.Use(tag("late"))
		},
		"nil Use":                    func(a *atlas.App) { a.Use(nil) },
		"nil route middleware":       func(a *atlas.App) { a.Get("/x", ok, nil) },
		"path without slash":         func(a *atlas.App) { a.Get("x", ok) },
		"prefix without slash":       func(a *atlas.App) { a.Group("x") },
		"prefix with trailing slash": func(a *atlas.App) { a.Group("/x/") },
		"duplicate route":            func(a *atlas.App) { a.Get("/x", ok); a.Get("/x", ok) },
		"conflicting wildcards": func(a *atlas.App) {
			a.Get("/{a}/x", ok)
			a.Get("/y/{b}", ok)
			a.Get("/{c}/{d}", ok)
			a.Get("/y/x", ok)
			a.Get("/{e}/x", ok)
		},
		"nil handler":                 func(a *atlas.App) { a.Get("/x", nil) },
		"nil standard handler":        func(a *atlas.App) { a.Handle("/x", nil) },
		"mount without prefix":        func(a *atlas.App) { a.Mount("", std) },
		"mount with wildcard":         func(a *atlas.App) { a.Mount("/{tenant}", std) },
		"mount with nil handler":      func(a *atlas.App) { a.Mount("/x", nil) },
		"mount under wildcard prefix": func(a *atlas.App) { a.Group("/{tenant}").Mount("/x", std) },
	}
	for name, register := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Error("registration did not panic")
				}
			}()
			register(quietApp())
		})
	}
}

func TestAppRunServesOpsEndpointsAndMetricsByRoute(t *testing.T) {
	t.Parallel()
	ln := listenLocal(t)
	logs := newLogSink()
	app := atlas.New(
		atlas.WithListener(ln),
		atlas.WithoutGlobalTelemetry(),
		atlas.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))),
	)
	app.Group("/v1").Get("/orders/{id}", text("order"))
	app.Post("/health", text("app health"))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	logs.waitFor(t, `"msg":"listening"`)
	url := "http://" + ln.Addr().String()

	if got := get(t, url+"/v1/orders/42"); got.body != "order" || got.header.Get("X-Request-ID") == "" {
		t.Errorf("GET /v1/orders/42 = %d %q %v", got.status, got.body, got.header)
	}
	for _, path := range []string{"/health", "/ping", "/liveness", "/readiness"} {
		if got := get(t, url+path); got.status != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, got.status)
		}
	}
	resp, err := http.Post(url+"/health", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "app health" {
		t.Errorf("POST /health = %q, want the app's handler", body)
	}
	if got := get(t, url+"/nope"); got.status != http.StatusNotFound || !strings.Contains(got.body, `"request_id":"`) {
		t.Errorf("GET /nope = %d %s, want a problem with the request ID", got.status, got.body)
	}
	if metrics := get(t, url+"/metrics").body; !strings.Contains(metrics, `http_route="/v1/orders/{id}"`) {
		t.Errorf("metrics lack the route:\n%s", metrics)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() = %v, want nil", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Run did not return")
	}
}

func TestAppRunRejectsRoutesShadowedByOpsEndpoints(t *testing.T) {
	t.Parallel()
	ok := text("ok")
	pprof := atlas.WithOpsHandler("/debug/", http.NotFoundHandler())
	tests := []struct {
		name     string
		opts     []atlas.Option
		register func(*atlas.App)
		wantErr  string
	}{
		{"GET /health", nil, func(a *atlas.App) { a.Get("/health", ok) }, `route "/health" clashes`},
		{"group root", nil, func(a *atlas.App) { a.Group("/ping").Get("/", ok) }, `route "/ping" clashes`},
		{"any method", nil, func(a *atlas.App) { a.Handle("/metrics", http.NotFoundHandler()) }, `route "/metrics" clashes`},
		{"mount", nil, func(a *atlas.App) { a.Mount("/liveness", http.NotFoundHandler()) }, `route "/liveness" clashes`},
		{"ops subtree", []atlas.Option{pprof}, func(a *atlas.App) { a.Post("/debug/x", ok) }, `clashes with ops handler "/debug/"`},
		{"mount over ops subtree", []atlas.Option{pprof}, func(a *atlas.App) { a.Mount("/debug", http.NotFoundHandler()) }, `clashes with ops handler "/debug/"`},
		{"mount above ops handler", []atlas.Option{atlas.WithOpsHandler("/tools/pprof/", http.NotFoundHandler())},
			func(a *atlas.App) { a.Mount("/tools", http.NotFoundHandler()) }, `clashes with ops handler "/tools/pprof/"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ln := listenLocal(t)
			app := quietApp(append(tt.opts, atlas.WithListener(ln))...)
			tt.register(app)

			err := app.Run(t.Context())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Run() = %v, want error containing %q", err, tt.wantErr)
			}
			if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
				t.Errorf("listener still open: Accept() = %v", err)
			}
		})
	}
}

func TestAdminServerFreesOpsPathsForTheApp(t *testing.T) {
	t.Parallel()
	ln, adminLn := listenLocal(t), listenLocal(t)
	logs := newLogSink()
	app := atlas.New(
		atlas.WithListener(ln),
		atlas.WithAdminListener(adminLn),
		atlas.WithoutGlobalTelemetry(),
		atlas.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))),
	)
	app.Get("/health", text("app health"))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = app.Run(ctx) }()
	logs.waitFor(t, `"msg":"listening"`)

	if got := get(t, "http://"+ln.Addr().String()+"/health"); got.body != "app health" {
		t.Errorf("app GET /health = %q", got.body)
	}
	if got := get(t, "http://"+adminLn.Addr().String()+"/health"); got.status != http.StatusOK || got.body == "app health" {
		t.Errorf("admin GET /health = %d %q", got.status, got.body)
	}
}

// derivesRequest is the usual shape of tenant or auth middleware.
func derivesRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		type tenantKey struct{}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tenantKey{}, "acme")))
	})
}

// startApp runs the App that build returns, given the options that isolate it for a test.
func startApp(t *testing.T, build func(...atlas.Option) *atlas.App, opts ...atlas.Option) (string, *logSink) {
	t.Helper()
	ln := listenLocal(t)
	logs := newLogSink()
	app := build(append([]atlas.Option{
		atlas.WithListener(ln),
		atlas.WithoutGlobalTelemetry(),
		atlas.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))),
	}, opts...)...)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	logs.waitFor(t, `"msg":"listening"`)
	return "http://" + ln.Addr().String(), logs
}

func TestRouteSurvivesMiddlewareThatDerivesTheRequest(t *testing.T) {
	t.Parallel()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	seen := make(chan string, 3)
	url, _ := startApp(t, func(opts ...atlas.Option) *atlas.App {
		app := atlas.New(opts...)
		app.Use(derivesRequest, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Pattern
				next.ServeHTTP(w, r)
			})
		})
		app.Get("/", text("root"))
		app.Group("/v1", derivesRequest).Get("/orders/{id}", text("order"))
		return app
	}, atlas.WithTracerProvider(tp))

	get(t, url+"/v1/orders/42")
	if got := <-seen; got != "GET /v1/orders/{id}" {
		t.Errorf("r.Pattern in app middleware = %q", got)
	}
	get(t, url+"/")
	<-seen
	get(t, url+"/nope")
	<-seen

	var names []string
	for _, s := range spans.Ended() {
		names = append(names, s.Name())
	}
	want := "GET /v1/orders/{id},GET /,GET"
	if got := strings.Join(names, ","); got != want {
		t.Errorf("span names = %q, want %q", got, want)
	}
	metrics := get(t, url+"/metrics").body
	for _, route := range []string{`http_route="/v1/orders/{id}"`, `http_route="/"`} {
		if !strings.Contains(metrics, route) {
			t.Errorf("metrics lack %s:\n%s", route, metrics)
		}
	}
	if strings.Contains(metrics, "{$}") {
		t.Errorf("metrics expose routing syntax:\n%s", metrics)
	}
}

func TestAbortedResponseIsStillRecorded(t *testing.T) {
	t.Parallel()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	url, logs := startApp(t, func(opts ...atlas.Option) *atlas.App {
		app := atlas.New(opts...)
		app.Get("/stream/{id}", func(w http.ResponseWriter, _ *http.Request) error {
			_, _ = io.WriteString(w, strings.Repeat("x", 1<<16))
			return errors.New("stream broke")
		})
		return app
	}, atlas.WithTracerProvider(tp))

	resp, err := http.Get(url + "/stream/1")
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Error("client read a complete response, want a broken one")
	}
	logs.waitFor(t, `"aborted":true`)

	metrics := get(t, url+"/metrics").body
	if !strings.Contains(metrics, `http_route="/stream/{id}"`) {
		t.Errorf("metrics lack the aborted request:\n%s", metrics)
	}
	var found bool
	for _, s := range spans.Ended() {
		found = found || s.Name() == "GET /stream/{id}"
	}
	if !found {
		t.Error("no span named after the route")
	}
}

// The tests below replace slog's and log's process-wide defaults, so they cannot run in parallel.

func restoreLogDefaults(t *testing.T) {
	t.Helper()
	prev, writer, flags := slog.Default(), log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
}

func TestHandlerLogsCarryRequestAndTraceIDs(t *testing.T) {
	restoreLogDefaults(t)
	sink := newLogSink()
	slog.SetDefault(slog.New(slog.NewJSONHandler(sink, nil)))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /work", func(_ http.ResponseWriter, r *http.Request) {
		slog.InfoContext(r.Context(), "working")
	})
	svc := startWith(t, mux)

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	got := get(t, svc.url+"/work", "traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")

	logs := sink.records(t, "working")
	if len(logs) != 1 {
		t.Fatalf("working logs = %v, want one in the application's own default:\n%s", logs, sink)
	}
	if logs[0]["trace_id"] != traceID || logs[0]["request_id"] != got.header.Get("X-Request-ID") {
		t.Errorf("log = %v, want trace_id %s and request_id %s", logs[0], traceID, got.header.Get("X-Request-ID"))
	}
	svc.logs.waitFor(t, `"msg":"request"`)
	access := svc.logs.records(t, "request")
	if len(access) != 1 || access[0]["trace_id"] != traceID {
		t.Errorf("access log = %v, want trace_id %s", access, traceID)
	}
}

func TestDefaultLoggerIsWrappedOnce(t *testing.T) {
	restoreLogDefaults(t)
	sink := newLogSink()
	slog.SetDefault(slog.New(slog.NewJSONHandler(sink, nil)))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /work", func(_ http.ResponseWriter, r *http.Request) {
		slog.InfoContext(r.Context(), "working")
	})
	// Two Runs in one process, the second also passing the already wrapped default as its logger.
	first := startWith(t, helloMux())
	if err := first.stop(); err != nil {
		t.Fatal(err)
	}
	ln := listenLocal(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- atlas.Run(ctx, mux, atlas.WithListener(ln), atlas.WithLogger(slog.Default())) }()
	defer func() {
		cancel()
		<-done
	}()
	sink.waitFor(t, `"msg":"listening"`)
	get(t, "http://"+ln.Addr().String()+"/work")
	sink.waitFor(t, `"msg":"request"`)

	if n := strings.Count(sink.String(), `"request_id"`); n != 2 { // one for "working", one for the access log
		t.Errorf("request_id appears %d times, want 2, each record once:\n%s", n, sink)
	}
}

func TestWithoutGlobalTelemetryKeepsSlogDefault(t *testing.T) {
	restoreLogDefaults(t)
	before := slog.Default()
	svc := start(t, helloMux())
	get(t, svc.url+"/hello")
	if slog.Default() != before {
		t.Error("WithoutGlobalTelemetry replaced slog's default logger")
	}
}

func TestDefaultLoggerInstalledOnlyWithoutOwnLogger(t *testing.T) {
	restoreLogDefaults(t)
	before := slog.Default()

	own := startWith(t, helloMux())
	if err := own.stop(); err != nil {
		t.Fatal(err)
	}
	if slog.Default() != before {
		t.Error("WithLogger replaced slog's initial default logger")
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- atlas.Run(ctx, helloMux(), atlas.WithListener(listenLocal(t))) }()
	deadline := time.Now().Add(waitTimeout)
	for slog.Default() == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if slog.Default() == before {
		t.Error("Run without a logger did not install its own as slog's default")
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run() = %v, want nil", err)
	}
}

// wrapperHandler stands for a logging decorator around another handler.
type wrapperHandler struct{ slog.Handler }

func TestLoggerWritingThroughPackageLogDoesNotDeadlock(t *testing.T) {
	for name, logger := range map[string]func() *slog.Logger{
		"initial default": slog.Default,
		"wrapped initial default": func() *slog.Logger {
			return slog.New(wrapperHandler{slog.Default().Handler()})
		},
	} {
		t.Run(name, func(t *testing.T) {
			restoreLogDefaults(t)
			buf := newLogSink()
			log.SetOutput(buf)
			log.SetFlags(0)
			ctx, cancel := context.WithCancel(t.Context())
			stopped := make(chan error, 1)
			go func() {
				stopped <- atlas.Run(ctx, helloMux(), atlas.WithListener(listenLocal(t)), atlas.WithLogger(logger()))
			}()
			buf.waitFor(t, "listening")

			done := make(chan struct{})
			go func() {
				slog.Info("through slog")
				log.Print("through log")
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(waitTimeout):
				t.Fatal("logging deadlocked")
			}
			if !strings.Contains(buf.String(), "through slog") || !strings.Contains(buf.String(), "through log") {
				t.Errorf("output = %q", buf.String())
			}
			cancel()
			if err := <-stopped; err != nil {
				t.Errorf("Run() = %v, want nil", err)
			}
		})
	}
}

func TestDecode(t *testing.T) {
	t.Parallel()
	type input struct {
		Name string `json:"name"`
		Qty  int    `json:"qty"`
	}
	tests := []struct {
		name        string
		body        string
		contentType string
		limit       int64
		wantStatus  int
		wantDetail  string
	}{
		{name: "valid", body: `{"name":"a","qty":2}`, contentType: "application/json", wantStatus: 200},
		{name: "charset", body: `{"name":"a","qty":2}`, contentType: "application/json; charset=utf-8", wantStatus: 200},
		{name: "json suffix", body: `{"name":"a","qty":2}`, contentType: "application/vnd.api+json", wantStatus: 200},
		{name: "unknown fields are ignored", body: `{"name":"a","qty":2,"x":1}`, contentType: "application/json", wantStatus: 200},
		{name: "trailing whitespace", body: "{\"name\":\"a\",\"qty\":2}\n ", contentType: "application/json", wantStatus: 200},
		{name: "no content type", body: `{}`, wantStatus: 415, wantDetail: "Content-Type"},
		{name: "wrong content type", body: `{}`, contentType: "text/plain", wantStatus: 415, wantDetail: "Content-Type"},
		{name: "broken content type", body: `{}`, contentType: "/", wantStatus: 415, wantDetail: "Content-Type"},
		{name: "empty", body: "", contentType: "application/json", wantStatus: 400, wantDetail: "empty"},
		{name: "invalid", body: `{"name":`, contentType: "application/json", wantStatus: 400, wantDetail: "not valid JSON"},
		{name: "wrong type", body: `{"qty":"x"}`, contentType: "application/json", wantStatus: 400, wantDetail: `field "qty"`},
		{name: "two values", body: `{} {}`, contentType: "application/json", wantStatus: 400, wantDetail: "single JSON value"},
		{name: "trailing garbage", body: `{} x`, contentType: "application/json", wantStatus: 400, wantDetail: "single JSON value"},
		{name: "over the limit", body: `{"name":"aaaaaaaaaaaaaaaaaaaa"}`, contentType: "application/json", limit: 10, wantStatus: 413, wantDetail: "exceeds 10 bytes"},
		{name: "second value over the limit", body: `{}                         {}`, contentType: "application/json", limit: 10, wantStatus: 413, wantDetail: "exceeds 10 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app := quietApp()
			app.Post("/", func(w http.ResponseWriter, r *http.Request) error {
				var (
					in  input
					err error
				)
				if tt.limit > 0 {
					in, err = atlas.DecodeLimit[input](w, r, tt.limit)
				} else {
					in, err = atlas.Decode[input](w, r)
				}
				if err != nil {
					if in != (input{}) {
						t.Errorf("value on error = %+v, want zero", in)
					}
					return err
				}
				return atlas.JSON(w, http.StatusOK, in)
			})

			req := httptest.NewRequest("POST", "/", strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusOK {
				if got := rec.Body.String(); got != `{"name":"a","qty":2}`+"\n" {
					t.Errorf("body = %q", got)
				}
				return
			}
			if p := decodeProblem(t, rec); !strings.Contains(p.Detail, tt.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q", p.Detail, tt.wantDetail)
			}
		})
	}
}

type failingBody struct{ err error }

func (b failingBody) Read([]byte) (int, error) { return 0, b.err }
func (failingBody) Close() error               { return nil }

type netFailure struct{ timeout bool }

func (netFailure) Error() string   { return "read tcp 10.0.3.7:8080->10.0.3.8:5000: broken" }
func (e netFailure) Timeout() bool { return e.timeout }
func (netFailure) Temporary() bool { return false }

func TestDecodeFailures(t *testing.T) {
	t.Parallel()
	type input struct{ Name string }
	decode := func(w http.ResponseWriter, r *http.Request) error {
		_, err := atlas.Decode[input](w, r)
		return err
	}
	send := func(body io.ReadCloser) *httptest.ResponseRecorder {
		app := quietApp()
		app.Post("/", decode)
		req := httptest.NewRequest("POST", "/", nil)
		req.Header.Set("Content-Type", "application/json")
		req.Body = body
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		return rec
	}

	for name, tt := range map[string]struct {
		body io.ReadCloser
		want int
	}{
		"nil body":     {nil, http.StatusBadRequest},
		"read timeout": {failingBody{netFailure{timeout: true}}, http.StatusRequestTimeout},
		"read failure": {failingBody{netFailure{}}, http.StatusInternalServerError},
	} {
		rec := send(tt.body)
		if rec.Code != tt.want {
			t.Errorf("%s: status = %d %s, want %d", name, rec.Code, rec.Body, tt.want)
		}
		if strings.Contains(rec.Body.String(), "10.0.3.7") {
			t.Errorf("%s: response leaks the address: %s", name, rec.Body)
		}
	}

	// Neither the Go type name nor the decoder's wording reaches the client.
	rec := send(io.NopCloser(strings.NewReader(`{"Name":1}`)))
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "input") || strings.Contains(rec.Body.String(), "json:") {
		t.Errorf("response = %d %s, want a 400 without internals", rec.Code, rec.Body)
	}
}

func TestJSONEncodingErrorWritesNothing(t *testing.T) {
	t.Parallel()
	app := quietApp()
	app.Get("/", func(w http.ResponseWriter, _ *http.Request) error {
		return atlas.JSON(w, http.StatusCreated, make(chan int))
	})
	rec := serveApp(app, "GET", "/", nil)
	decodeProblem(t, rec)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}
