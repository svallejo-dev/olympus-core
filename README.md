# olympus-core

SDKs de Go para resolver lo que cada servicio vuelve a escribir: arrancar la app como API
con valores de producción y llamar a otras APIs con resiliencia. Proyecto hermano de
[keel](https://github.com/svallejo-dev/keel-cli): misma filosofía de defaults sensatos y
cero ceremonia, pero exclusivo para Go.

| SDK | Para qué | Instalar |
|---|---|---|
| [`atlas-server`](atlas-server) | Arranca la app como API (`:8080`), con `/health`, `/ping`, `/liveness`, `/readiness`, `/metrics`, recuperación de panics, access log, OpenTelemetry y apagado ordenado | `go get github.com/svallejo-dev/olympus-core/atlas-server@latest` |
| [`hermes-http`](hermes-http) | Llama a otras APIs con circuit breaker, reintentos con backoff y jitter, timeouts, bulkhead, rate limit y OpenTelemetry | `go get github.com/svallejo-dev/olympus-core/hermes-http@latest` |

Atlas sostiene tu app; hermes lleva sus mensajes a otras APIs. Los próximos SDKs seguirán
con figuras del Olimpo que cualquiera reconoce.

## Principios

- **Independientes.** Cada SDK es un módulo con su propio `go.mod`: instalas solo el que
  usas y no arrastras las dependencias del otro. Ningún SDK importa a otro, y la CI lo
  comprueba (`scripts/check-independence.sh`).
- **Se combinan por estándares, no por imports.** atlas configura los proveedores globales
  de OpenTelemetry; hermes los usa por defecto. Usados juntos, las métricas de hermes salen
  en el `/metrics` de atlas y las trazas quedan enlazadas, sin que uno conozca al otro.
- **Wrappers de librerías probadas.** `net/http`, OpenTelemetry, `prometheus/client_golang`,
  `sony/gobreaker`, `x/time/rate`. Lo nuestro es la configuración y el pegamento.
- **Defaults de producción, todo configurable** con opciones funcionales.
- **Go idiomático**, sin frameworks que tomen el control: aceptan `http.Handler` y
  `*http.Client` estándar.

## Instalación (repo privado)

```sh
go env -w GOPRIVATE=github.com/svallejo-dev/*
git config --global url."git@github.com:".insteadOf "https://github.com/"   # o un token HTTPS
go get github.com/svallejo-dev/olympus-core/atlas-server@latest
```

## Versiones

Cada SDK se versiona por separado con tags por carpeta, la convención de Go para módulos en
subdirectorios:

```sh
git tag -a atlas-server/v0.4.0 -m "atlas-server v0.4.0" && git push origin atlas-server/v0.4.0
```

El workflow `release` corre los tests de ese módulo y publica un GitHub Release con los
commits que tocaron su carpeta. `go get .../atlas-server@v0.4.0` resuelve ese tag.

## Desarrollo

```sh
go work sync                                   # go.work une los módulos en local
cd atlas-server && GOWORK=off go test -race ./...      # cada SDK se prueba solo, como lo instala un usuario
golangci-lint run --config=../.golangci.yml ./...
./scripts/check-independence.sh
go run ./examples/orders-api                   # atlas + hermes juntos
```

`examples/` es un módulo aparte que usa los dos SDKs desde el código local (`replace`); no se
publica.
