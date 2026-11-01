// Command orders-api serves orders enriched with product data from a catalog API, using
// atlas to run the service and hermes to call the catalog.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/svallejo-dev/olympus/atlas"
	"github.com/svallejo-dev/olympus/hermes"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("orders-api stopped", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	catalogURL := os.Getenv("CATALOG_URL")
	if catalogURL == "" {
		catalogURL = "http://localhost:8081"
	}
	catalog, err := hermes.New(catalogURL,
		hermes.WithTimeout(5*time.Second),
		hermes.WithLogger(logger),
	)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("GET /orders/{id}", getOrder(catalog))

	return atlas.Run(context.Background(), mux,
		atlas.WithLogger(logger),
		atlas.WithServiceName("orders-api"),
		atlas.WithReadinessCheck("catalog", catalogReachable(catalog)),
	)
}

// catalogReachable keeps the instance out of rotation while the catalog API is unreachable.
func catalogReachable(catalog *hermes.Client) atlas.Check {
	return func(ctx context.Context) error {
		_, err := hermes.GetJSON[struct{}](ctx, catalog, "/livez")
		return err
	}
}
