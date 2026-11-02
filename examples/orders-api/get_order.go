package main

import (
	"errors"
	"net/http"

	"github.com/svallejo-dev/olympus-core/atlas-server"
	"github.com/svallejo-dev/olympus-core/hermes-http"
)

type product struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Price int64  `json:"price_cents"`
}

type order struct {
	ID      string  `json:"id"`
	Product product `json:"product"`
}

// getOrder returns a fixed demo order whose product comes from the catalog API.
func getOrder(catalog *hermes.Client) atlas.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		id := r.PathValue("id")
		p, err := hermes.GetJSON[product](r.Context(), catalog, "/products/sku-"+id)
		if err != nil {
			return catalogError(id, err)
		}
		return atlas.JSON(w, http.StatusOK, order{ID: id, Product: p})
	}
}

func catalogError(id string, err error) error {
	if statusErr, ok := errors.AsType[*hermes.StatusError](err); ok && statusErr.StatusCode == http.StatusNotFound {
		return atlas.Errorf(http.StatusNotFound, "order %s not found", id)
	}
	if errors.Is(err, hermes.ErrCircuitOpen) {
		return atlas.Errorf(http.StatusServiceUnavailable, "catalog unavailable: %w", err)
	}
	return atlas.Errorf(http.StatusBadGateway, "catalog error: %w", err)
}
