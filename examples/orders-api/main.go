// Command orders-api serves orders enriched with product data from a catalog API, using
// atlas for the API and hermes to call the catalog.
package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/svallejo-dev/olympus-core/atlas-server"
	"github.com/svallejo-dev/olympus-core/hermes-http"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	catalogURL := os.Getenv("CATALOG_URL")
	if catalogURL == "" {
		catalogURL = "http://localhost:8081"
	}
	catalog, err := hermes.New(catalogURL, hermes.WithTimeout(5*time.Second))
	if err != nil {
		return err
	}

	app := atlas.New(
		atlas.WithServiceName("orders-api"),
		atlas.WithReadinessCheck("catalog", catalogReachable(catalog)),
	)
	v1 := app.Group("/v1")
	v1.Get("/orders/{id}", getOrder(catalog))

	return app.Run(context.Background())
}

// catalogReachable keeps the instance out of rotation while the catalog API is unreachable.
func catalogReachable(catalog *hermes.Client) atlas.Check {
	return func(ctx context.Context) error {
		_, err := hermes.Get[struct{}](ctx, catalog, "/liveness")
		return err
	}
}
