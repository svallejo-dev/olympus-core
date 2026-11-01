# orders-api

Ejemplo de atlas y hermes juntos: una API de pedidos que completa cada pedido con datos de
una API de catálogo.

```sh
CATALOG_URL=http://localhost:8081 go run ./examples/orders-api
curl localhost:8080/orders/42      # llama a GET /products/sku-42 del catálogo con reintentos y breaker
curl localhost:8080/readiness         # 503 mientras el catálogo no responda
curl localhost:8080/metrics        # métricas del servidor (atlas) y de las llamadas salientes (hermes)
```
