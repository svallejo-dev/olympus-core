package atlas_test

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/svallejo-dev/olympus/atlas"
)

func ExampleRun() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "hello, request %s\n", atlas.RequestID(r.Context()))
	})

	if err := atlas.Run(context.Background(), mux); err != nil {
		log.Fatal(err)
	}
}

func ExampleWithReadinessCheck() {
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}

	err = atlas.Run(context.Background(), http.NewServeMux(),
		atlas.WithReadinessCheck("postgres", db.PingContext),
	)
	if err != nil {
		log.Fatal(err)
	}
}

func ExampleWithShutdownHook() {
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}

	err = atlas.Run(context.Background(), http.NewServeMux(),
		atlas.WithShutdownDelay(5*time.Second),
		atlas.WithShutdownHook("postgres", func(context.Context) error { return db.Close() }),
	)
	if err != nil {
		log.Fatal(err)
	}
}

func ExampleWithBackground() {
	consume := func(ctx context.Context) error {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				// Poll the queue; returning an error here stops the whole service.
			}
		}
	}

	err := atlas.Run(context.Background(), http.NewServeMux(),
		atlas.WithBackground("queue-consumer", consume),
	)
	if err != nil {
		log.Fatal(err)
	}
}

func ExampleWithOpsHandler() {
	err := atlas.Run(context.Background(), http.NewServeMux(),
		atlas.WithAdminAddr(":9090"),
		atlas.WithOpsHandler("/debug/pprof/", http.HandlerFunc(pprof.Index)),
		atlas.WithOpsHandler("/debug/pprof/profile", http.HandlerFunc(pprof.Profile)),
	)
	if err != nil {
		log.Fatal(err)
	}
}

func ExampleWithPrometheusGatherer() {
	err := atlas.Run(context.Background(), http.NewServeMux(),
		atlas.WithPrometheusGatherer(prometheus.DefaultGatherer),
	)
	if err != nil {
		log.Fatal(err)
	}
}

// Server-Sent Events, long polls and WebSockets outlive http.Server.Shutdown unless told to stop.
func ExampleWithServerConfig() {
	shuttingDown := make(chan struct{})
	// With WithAdminAddr the hook is registered on both servers; once is enough.
	notify := sync.OnceFunc(func() { close(shuttingDown) })

	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		_ = rc.SetWriteDeadline(time.Time{})
		for {
			select {
			case <-shuttingDown:
				return
			case <-r.Context().Done():
				return
			case <-time.After(15 * time.Second):
				_, _ = fmt.Fprint(w, ": keep-alive\n\n")
				_ = rc.Flush()
			}
		}
	})

	err := atlas.Run(context.Background(), mux,
		atlas.WithServerConfig(func(s *http.Server) { s.RegisterOnShutdown(notify) }),
	)
	if err != nil {
		log.Fatal(err)
	}
}
