package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/svallejo-dev/olympus/hermes"
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
func getOrder(catalog *hermes.Client) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		p, err := hermes.GetJSON[product](r.Context(), catalog, "/products/sku-"+id)
		var statusErr *hermes.StatusError
		switch {
		case errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound:
			http.Error(w, "order not found", http.StatusNotFound)
			return
		case errors.Is(err, hermes.ErrCircuitOpen):
			http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
			return
		case err != nil:
			http.Error(w, "catalog error", http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(order{ID: id, Product: p})
	})
}
