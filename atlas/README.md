# atlas

`atlas` arranca un servicio Go como API HTTP con valores de producción, para no reescribir el
mismo `main` en cada proyecto. Recibe tu `http.Handler` (un `http.ServeMux`, chi, el engine de
Gin...) y se encarga del resto:

- escucha en `:8080` (o `$PORT`) con timeouts de servidor razonables;
- expone `/health`, `/ping`, `/liveness`, `/readiness` y `/metrics`;
- recupera panics, asigna un request ID y escribe un access log con `slog`;
- instrumenta con OpenTelemetry (métricas en Prometheus, trazas por OTLP);
- ejecuta trabajos en segundo plano atados al ciclo de vida del servicio;
- se apaga de forma ordenada con SIGINT/SIGTERM, compatible con Kubernetes.

No impone framework: todo lo que añade son middlewares estándar alrededor de tu handler.

## Instalación

```sh
go get github.com/svallejo-dev/olympus-core/atlas@latest
```

El repositorio es privado. Indica a Go que no use el proxy público y da acceso a git por SSH o
con un token:

```sh
go env -w GOPRIVATE='github.com/svallejo-dev/*'
git config --global url."git@github.com:".insteadOf "https://github.com/"
```

## Inicio rápido

```go
package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/svallejo-dev/olympus-core/atlas"
)

func main() {
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hola, request " + atlas.RequestID(r.Context())))
	})

	err = atlas.Run(context.Background(), mux,
		atlas.WithServiceName("orders"),
		atlas.WithReadinessCheck("postgres", db.PingContext),
		atlas.WithShutdownHook("postgres", func(context.Context) error { return db.Close() }),
	)
	if err != nil {
		log.Fatal(err)
	}
}
```

`Run` bloquea hasta que llega una señal, se cancela el contexto o falla un servidor o un trabajo en
segundo plano, y devuelve `nil` si el apagado fue limpio. Con `handler` a `nil` sirve
`http.DefaultServeMux`, como `http.ListenAndServe(addr, nil)`.

Una configuración inválida hace que `Run` devuelva un error antes de escuchar: checks de readiness
con nombre repetido, nombres vacíos, checks, hooks, trabajos o handlers `nil`, y patrones de
`WithOpsHandler` no válidos. El error enumera todos los problemas a la vez, y los listeners de
`WithListener` y `WithAdminListener` se cierran igualmente.

## Endpoints

| Ruta | Responde | Uso |
|---|---|---|
| `GET /health` | el informe detallado de los checks (`200` o `503`), sin mirar si el servicio está drenando | diagnóstico, dashboards |
| `GET /ping` | `200 pong`, sin mirar nada más | comprobar a mano o desde un balanceador que el servicio responde |
| `GET /liveness` | `200 {"status":"ok"}` mientras el proceso vive | liveness probe |
| `GET /readiness` | `503` antes de empezar a servir y durante el apagado; si no, ejecuta los checks: `200` o `503` con `ok`/`fail` por check | readiness probe / balanceador |
| `GET /metrics` | métricas en formato Prometheus | scraping |

`/readiness` solo dice qué check falla; `/health` añade el mensaje de error, que puede llevar
direcciones internas o detalles de la conexión:

```json
{"status":"unavailable","checks":{"postgres":"fail","cache":"ok"}}
{"status":"unavailable","checks":{"postgres":"error: dial tcp 10.0.3.7:5432: connection refused","cache":"ok"}}
```

- Los checks se ejecutan en paralelo, cada uno con un timeout de 2 s; un panic en un check cuenta
  como error. Si un check ignora el contexto y sigue en marcha tras el timeout, las sondas
  siguientes lo dan por fallido (`still running after timeout`) sin lanzar otra goroutine hasta que
  termine; las sondas que llegan mientras un check está en curso esperan a esa misma ejecución.
- `/liveness` no ejecuta checks a propósito: si la base de datos cae, reiniciar el pod no la arregla
  y Kubernetes acabaría reiniciando todas las réplicas a la vez. La dependencia caída se refleja
  en `/readiness`, que saca el pod del balanceo sin matarlo.
- En el servidor compartido, atlas mira el método y el path exactos antes de llamar a tu handler:
  `GET` o `HEAD` sobre uno de estos cinco paths los atiende atlas; todo lo demás (otros métodos,
  `/liveness/`, `//liveness`...) llega a tu app tal cual, sin limpiar el path ni redirigir.
- Con `WithAdminAddr(":9090")` estos endpoints se sirven **solo** en ese puerto y tu handler queda
  solo en el principal. Úsalo si no quieres exponer `/metrics` ni `/health` al exterior.

### Ampliar el servidor de operación

`WithOpsHandler(pattern, h)` añade endpoints al servidor de operación (el compartido o el de
`WithAdminAddr`), por ejemplo `net/http/pprof`:

```go
atlas.Run(ctx, mux,
	atlas.WithAdminAddr(":9090"),
	atlas.WithOpsHandler("/debug/pprof/", http.HandlerFunc(pprof.Index)),
	atlas.WithOpsHandler("/debug/pprof/profile", http.HandlerFunc(pprof.Profile)),
)
```

- El patrón es un path limpio, sin método, host ni comodines: coincide exacto o, si acaba en `/`,
  con todo lo que cuelga de él (gana el más largo). Atiende cualquier método.
- Sigue la misma regla que los endpoints de serie: atlas compara el path delante de tu app, que
  recibe todo lo demás intacto. No se pueden usar `/`, los cuatro paths de serie ni repetir un
  patrón.
- En el servidor compartido esos endpoints quedan en el puerto público; para pprof usa
  `WithAdminAddr`.

`WithPrometheusGatherer(g)` hace que `/metrics` sirva también un registro de Prometheus existente,
por ejemplo `prometheus.DefaultGatherer` si tu código usa `promauto`. Se mezcla con el registro de
atlas; si un nombre de métrica aparece en los dos (las de Go y de proceso, que
`DefaultGatherer` también trae), se queda la de atlas.

## Valores por defecto

| Ajuste | Por defecto | Cómo cambiarlo |
|---|---|---|
| Dirección | `:$PORT` si `PORT` está definida, si no `:8080` | `WithAddr` (gana a `PORT`), `WithListener` |
| Endpoints de operación | en el mismo servidor que la app | `WithAdminAddr`, `WithAdminListener` |
| Logger | JSON a stdout (no toca `slog.SetDefault`) | `WithLogger` |
| `service.name` | nombre del binario (`os.Args[0]`) | `WithServiceName`; `OTEL_SERVICE_NAME` y `OTEL_RESOURCE_ATTRIBUTES` ganan al código |
| Retardo de drenado | `5s` si existe `KUBERNETES_SERVICE_HOST`, si no `0` | `WithShutdownDelay` |
| Timeout de apagado | `20s` | `WithShutdownTimeout` |
| Servidor HTTP | `ReadHeaderTimeout` 10 s, `ReadTimeout` 30 s, `WriteTimeout` 30 s, `IdleTimeout` 120 s, `MaxHeaderBytes` 1 MiB, `ErrorLog` hacia `slog` | `WithServerConfig(func(*http.Server))` |
| Access log | activado | `WithoutAccessLog` |
| Trazas | sin exportador; OTLP/HTTP si hay `OTEL_EXPORTER_OTLP_ENDPOINT` o `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | `WithTracerProvider` |
| Métricas | `MeterProvider` del SDK con exportador Prometheus | `WithMeterProvider` |
| Globales de OpenTelemetry | se configuran | `WithoutGlobalTelemetry` |

Para streaming (SSE, descargas largas) sube o anula `WriteTimeout` con `WithServerConfig`, o usa
`http.ResponseController.SetWriteDeadline` dentro del handler; y avísale del apagado como se
explica en [Conexiones de larga duración](#conexiones-de-larga-duración).

El access log escribe una línea por petición, también cuando se aborta: un panic después de enviar
las cabeceras, o un `panic(http.ErrAbortHandler)`, deja la línea con nivel `ERROR`,
`aborted=true` y el status ya enviado (o `500` si no se envió ninguno).

## Trabajo en segundo plano

`WithBackground(name, fn)` ejecuta `fn(ctx)` bajo el ciclo de vida de `Run`, para consumidores de
colas, refrescos periódicos y similares:

```go
atlas.WithBackground("orders-consumer", func(ctx context.Context) error {
	return consumer.Run(ctx) // vuelve con ctx.Err() cuando atlas lo cancela
})
```

- Arranca cuando los listeners ya están abiertos.
- Su `ctx` se cancela al empezar el apagado, **después** del retardo de drenado y a la vez que el
  `Shutdown` de los servidores: mientras el pod sale del balanceo sigue trabajando, como las
  peticiones. `Run` lo espera dentro del timeout de apagado, y los hooks corren después, así que
  puede usar la base de datos hasta el final.
- Si devuelve un error (o hace panic, que se recupera y se registra con su stack) antes del
  apagado, `Run` inicia el apagado y devuelve ese error. Si devuelve `nil`, simplemente termina.
- Durante el apagado, un error que envuelve `context.Canceled` cuenta como parada limpia; otros
  errores, o no terminar dentro del timeout, se suman al error de `Run`.

## Apagado

Al recibir SIGINT/SIGTERM (o al cancelar el contexto de `Run`, o si falla un servidor o un trabajo
en segundo plano):

1. Se deja de escuchar señales: un segundo Ctrl-C mata el proceso de inmediato.
2. `/readiness` pasa a `503` y se registra `shutting down`.
3. Se espera el retardo de drenado, para que el balanceador o Kubernetes dejen de enviar tráfico.
   Durante ese tiempo se siguen atendiendo peticiones y `/liveness` sigue en `200`.
4. A la vez: `Shutdown` de los servidores, que dejan de aceptar conexiones y esperan a las
   peticiones en curso, y cancelación del contexto de los trabajos en segundo plano, a los que se
   espera.
5. Hooks de apagado en orden inverso al de registro (como `defer`). Un panic en un hook se
   recupera y cuenta como error; los demás hooks y el paso 6 se ejecutan igual.
6. Flush y cierre de los providers de OpenTelemetry que creó atlas.
7. Se registra `stopped`.

Los pasos 4 a 6 comparten el timeout de apagado. Retardo + timeout (5 s + 20 s por defecto) debe
caber en el `terminationGracePeriodSeconds` del pod (30 s por defecto). Los errores de los
servidores, los trabajos, los hooks y la telemetría se devuelven juntos con `errors.Join`.

### Conexiones de larga duración

`http.Server.Shutdown` no sabe nada de las conexiones secuestradas con `Hijack` (WebSockets): no
las espera ni las cierra. Tampoco avisa a los handlers de SSE o long polling, que siguen hasta
que el timeout de apagado corta la conexión. atlas no cancela el contexto de las peticiones en
curso, porque eso abortaría también las peticiones normales que se están drenando. Avisa tú a
esos handlers con `RegisterOnShutdown`:

```go
shuttingDown := make(chan struct{})
notify := sync.OnceFunc(func() { close(shuttingDown) }) // con WithAdminAddr se registra en los dos servidores

atlas.Run(ctx, mux,
	atlas.WithServerConfig(func(s *http.Server) { s.RegisterOnShutdown(notify) }),
)
```

El handler de SSE hace `select` sobre `shuttingDown` y `r.Context().Done()` y vuelve; uno de
WebSocket envía un close frame y cierra la conexión. Las conexiones secuestradas que tengas que
esperar, espéralas en un `WithShutdownHook`.

## Telemetría

- **Métricas**: atlas crea un `MeterProvider` del SDK con el exportador Prometheus sobre un
  registro privado que además incluye los colectores de Go y de proceso. `/metrics` sirve ese
  registro: `http_server_request_duration_seconds` (con `http_route` si tu router es un
  `http.ServeMux`), `go_*`, `process_*` y todo lo que se mida con el provider.
- **Trazas**: si defines `OTEL_EXPORTER_OTLP_ENDPOINT` (o `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`),
  atlas crea un `TracerProvider` con exportador OTLP/HTTP y procesador batch. El resto de la
  configuración sale de las variables estándar (`OTEL_EXPORTER_OTLP_HEADERS`,
  `OTEL_TRACES_SAMPLER`...). Sin esas variables no se exporta nada, pero el contexto de traza
  entrante (`traceparent`) se propaga igual y su `trace_id` aparece en el access log.
- **Propagación**: W3C Trace Context + Baggage.
- **Providers propios**: `WithMeterProvider` y `WithTracerProvider` sustituyen a los de atlas, que
  entonces no los cierra al apagar. Con un `MeterProvider` propio, `/metrics` solo muestra las
  métricas de Go y de proceso (y las de `WithPrometheusGatherer`).
- **Globales**: salvo `WithoutGlobalTelemetry`, atlas registra como globales de OpenTelemetry el
  `MeterProvider`, el `TracerProvider` (solo si hay uno real), el propagador y un error handler
  que escribe en el logger.

Las globales son el punto de encuentro con otros SDKs del mismo proceso. `hermes`, el cliente HTTP
de olympus-core, usa por defecto `otel.GetMeterProvider()`, `otel.GetTracerProvider()` y el propagador
global; por eso, con atlas en el mismo binario, sus métricas de cliente salen en el mismo
`/metrics` y sus spans cuelgan de la traza de la petición entrante, sin que ninguno de los dos
módulos dependa del otro.

## Con Gin o chi

Cualquier `http.Handler` sirve. Con Gin, crea el engine con `gin.New()` (atlas ya pone recovery y
access log) y pásalo tal cual:

```go
r := gin.New()
r.GET("/hello", func(c *gin.Context) { c.String(http.StatusOK, "hola") })
err := atlas.Run(context.Background(), r)
```

Con chi:

```go
r := chi.NewRouter()
r.Get("/hello", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hola")) })
err := atlas.Run(context.Background(), r)
```

El `ResponseWriter` que recibe tu handler implementa `Unwrap`, `http.Flusher`, `http.Hijacker` e
`io.ReaderFrom`, así que `http.ResponseController`, streaming, WebSockets y el `sendfile` de
`http.ServeContent` siguen funcionando. Para que el apagado ordenado cubra WebSockets y SSE, mira
[Conexiones de larga duración](#conexiones-de-larga-duración).
