# hermes

Cliente HTTP para llamar a otras APIs con la resiliencia ya resuelta por configuración:
reintentos con backoff exponencial y jitter, timeouts por intento y totales, circuit breaker
por host, bulkhead, rate limiting y OpenTelemetry. Así cada proyecto deja de reimplementarlos.

Es un SDK independiente del monorepo `olympus-core`: no importa `atlas` ni ningún otro módulo.

## Instalación

```sh
go get github.com/svallejo-dev/olympus-core/hermes-http@latest
```

El repositorio es privado, así que Go necesita saltarse el proxy público y tener acceso por git:

```sh
go env -w GOPRIVATE='github.com/svallejo-dev/*'
# SSH:
git config --global url."git@github.com:".insteadOf "https://github.com/"
# o bien un token por HTTPS:
git config --global url."https://<usuario>:<token>@github.com/".insteadOf "https://github.com/"
```

## Uso rápido

```go
c, err := hermes.New("https://api.example.com/v1",
	hermes.WithUserAgent("billing/1.4"),
	hermes.WithBearerToken(tokens.Get), // func(ctx) (string, error), se llama en cada intento
)
if err != nil {
	return err
}

type User struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

u, err := hermes.GetJSON[User](ctx, c, "/users/42") // GET https://api.example.com/v1/users/42
if statusErr, ok := errors.AsType[*hermes.StatusError](err); ok && statusErr.StatusCode == http.StatusNotFound {
	return ErrUserNotFound
}
if err != nil {
	return fmt.Errorf("get user: %w", err)
}

created, err := hermes.PostJSON[User](ctx, c, "/users", User{Name: "Ada"},
	hermes.IdempotencyKey(requestID), // sin esta clave un POST no se reintenta
	hermes.Query(url.Values{"notify": {"true"}}),
)
```

- Los helpers JSON devuelven `*hermes.StatusError` para cualquier respuesta no-2xx, con
  `Method`, `URL` (sin credenciales ni query, se puede loguear), `StatusCode`, `Header` (las
  cabeceras de la respuesta) y los primeros 4 KiB de `Body`. Un 204 o un cuerpo vacío devuelven
  el valor cero de `T`.
- Para control total usa `c.NewRequest` + `c.Do`: igual que `net/http`, un no-2xx **no** es un
  error y tú cierras el body.
- Un error de transporte de `c.Do` o de los helpers es un `*url.Error` cuya `URL` también va sin
  credenciales ni query.
- Un timeout (de intento, total o el deadline del `ctx`) cumple
  `errors.Is(err, context.DeadlineExceeded)`; una cancelación del llamador, `context.Canceled`.
- El `path` se concatena a la ruta del base URL (`/v1` + `/users` = `/v1/users`) y se conservan
  la query del base URL y la del path. Un path absoluto (`https://...`) es un error.

## Valores por defecto

| Ajuste | Defecto | Opción |
|---|---|---|
| Timeout total (reintentos, redirecciones y lectura del body incluidos) | 30 s | `WithTimeout(d)`, `0` lo desactiva |
| Timeout por intento (hasta recibir las cabeceras) | 10 s | `WithAttemptTimeout(d)`, `0` lo desactiva |
| Intentos (incluido el primero) | 3 | `WithRetry(RetryPolicy{MaxAttempts: n})`, `WithoutRetry()` |
| Backoff | 100 ms inicial, ×2, tope 2 s, full jitter | `RetryPolicy.InitialBackoff`, `MaxBackoff` |
| `Retry-After` máximo que se respeta | 30 s | `RetryPolicy.MaxRetryAfter` |
| Breaker: apertura | 5 fallos seguidos, o ≥ 50 % de fallos con ≥ 20 peticiones en 60 s | `WithCircuitBreaker(BreakerPolicy{...})` |
| Breaker: abierto durante | 30 s, luego 1 petición de prueba | `BreakerPolicy.OpenTimeout`, `HalfOpenRequests` |
| Bulkhead | desactivado | `WithMaxConcurrent(n)` |
| Rate limit | desactivado | `WithRateLimit(porSegundo, burst)` |
| Tamaño máximo de respuesta JSON | 10 MiB | `WithMaxResponseBytes(n)` |
| Transporte base | clon de `http.DefaultTransport`, 100 conexiones idle por host | `WithTransport(rt)` |

Los campos a cero de `RetryPolicy` y `BreakerPolicy` toman el valor por defecto, así que solo
escribes los que cambias. Bulkhead y rate limit son por `Client` (todas las peticiones, todos
los hosts) y la espera respeta el `ctx`. El bulkhead cuenta llamadas: una llamada ocupa su hueco
durante todos sus reintentos hasta que se cierra el body. El rate limit marca el ritmo de los
**intentos**: cada reintento espera su propio token, y si el token llegaría después del deadline
falla al instante con un error que envuelve `context.DeadlineExceeded`, sin reintentar.

El timeout total es el `Timeout` del `http.Client` interno (y del que devuelve `HTTPClient()`):
cubre la llamada lógica entera, con reintentos, redirecciones y lectura del body. El timeout de
intento cuelga del contexto de la petición, así que nunca dura más que el deadline restante.

## Cuándo se reintenta

Se reintenta una petición cuando se cumple **todo** esto:

1. Quedan intentos y el `ctx` del llamador sigue vivo (cancelar corta en seco, sin reintentar).
2. El método es idempotente (`GET`, `HEAD`, `OPTIONS`, `PUT`, `DELETE`, `TRACE`), o la petición
   lleva cabecera `Idempotency-Key`, o la política tiene `RetryNonIdempotent: true`.
3. El body se puede rebobinar: sin body, o con `req.GetBody` (lo pone `http.NewRequest` para
   `bytes.Reader`, `bytes.Buffer` y `strings.Reader`; los helpers JSON siempre lo cumplen).
4. El resultado es reintentable: error de transporte (incluido un timeout de intento) o estado
   429, 502, 503 o 504. `RetryPolicy.ShouldRetry` sustituye esta clasificación.

Nunca se reintenta, diga lo que diga `ShouldRetry`, un rechazo del circuit breaker, un fallo de
`WithBearerToken` ni una espera del rate limit que no cabe en el deadline. Si la respuesta trae
`Retry-After` (segundos o fecha HTTP) se espera eso en lugar del backoff; si pide más de
`RetryPolicy.MaxRetryAfter` (30 s por defecto) o la espera no cabe en el deadline restante, se
para y se devuelve la última respuesta. Los bodies de las respuestas descartadas se drenan y
cierran para reutilizar la conexión.

## Circuit breaker

Hay un breaker por esquema, host y puerto (`https://api.example.com:443`; `api.example.com` y
`api.example.com:443` comparten breaker). Tras demasiados fallos se abre y las peticiones fallan
al instante con un error que envuelve `hermes.ErrCircuitOpen` (`errors.Is` funciona), sin tocar
la red; pasado `OpenTimeout` deja pasar una petición de prueba y, si va bien, se cierra.

Cuentan como fallo los errores de transporte, los 5xx y cualquier timeout: el de intento, el
total y el deadline del `ctx` del llamador, aunque sea más corto que el timeout de intento, así
que un upstream colgado abre el breaker. No cuentan los 4xx, las cancelaciones del llamador
(`context.Canceled`) ni los fallos de `WithBearerToken`.

Un `Client` guarda como mucho 1024 breakers; al llegar al tope descarta los cerrados, que no
tienen estado que merezca la pena, y conserva los abiertos y semiabiertos.

## Sin estado

`hermes` no guarda nada entre peticiones: sin cookie jar ni sesión, y la configuración es
inmutable tras `New`, así que un `Client` se comparte entre goroutines sin sincronización adicional. El
único estado es el inherente a la resiliencia: los breakers por host (y los contadores del
bulkhead y del rate limiter), en memoria y por proceso. No se comparte entre réplicas ni
sobrevive a un reinicio. Crea un `Client` por API de destino y reutilízalo.

## Telemetría

Cada llamada lógica produce **un** span de cliente (otelhttp), con la cabecera `traceparent`
inyectada una vez y compartida por todos los intentos; cada reintento añade un evento
`hermes.retry` al span. Métricas, además de las `http.client.*` de otelhttp:

| Métrica | Tipo | Atributos |
|---|---|---|
| `hermes.client.retries` | contador | `server.address`, `server.port`, `hermes.retry.reason` (`503`, `timeout`, `transport_error`…) |
| `hermes.client.circuit_breaker.state` | gauge (0 cerrado, 1 semiabierto, 2 abierto) | `server.address`, `server.port` |
| `hermes.client.circuit_breaker.rejections` | contador | `server.address`, `server.port` |

gobreaker cambia de estado de forma perezosa: el gauge pasa de abierto a semiabierto cuando llega
la siguiente petición tras `OpenTimeout`, no en el instante en que vence. Un host sin tráfico se
queda en 2 aunque su breaker ya admitiría una prueba.

Por defecto se usan los providers y el propagador **globales** de OpenTelemetry. Los globales
delegan, así que da igual si se registran antes o después de `hermes.New`. Por eso, en un
servicio arrancado con `atlas`, que registra los globales al iniciar, las métricas de hermes
aparecen en su `/metrics` y los spans se encadenan con los del servidor sin que ningún módulo
importe al otro. Para inyectarlos explícitamente: `WithMeterProvider`, `WithTracerProvider` y
`WithPropagators`. Los cambios de estado del breaker se loguean en `Warn` y los reintentos en
`Debug` con `slog.Default()` o el logger de `WithLogger`.

## Con clientes generados

`c.HTTPClient()` devuelve un `*http.Client` nuevo que usa la misma tubería (mismos breakers,
límites y telemetría), para SDKs generados que aceptan uno, por ejemplo oapi-codegen:

```go
api, err := orders.NewClientWithResponses("https://orders.internal",
	orders.WithHTTPClient(c.HTTPClient()),
)
```

Las URLs las construye el cliente generado, así que ahí no se aplica el base URL de hermes, pero
el resto de la tubería sí, timeout total incluido. Sus errores de transporte son los de
`net/http`, sin la limpieza de URL que hace `c.Do`.

`c.CloseIdleConnections()` (o `CloseIdleConnections()` sobre el `*http.Client` de `HTTPClient()`)
cierra las conexiones ociosas del transporte base, aunque otelhttp no reenvíe esa llamada.
