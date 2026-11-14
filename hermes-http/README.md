# hermes-http

Cliente HTTP para llamar a otras APIs con la resiliencia ya resuelta por configuración:
reintentos con backoff exponencial y jitter, timeouts por intento y totales, circuit breaker
por host, bulkhead, rate limiting y OpenTelemetry. Así cada proyecto deja de reimplementarlos.

Es un SDK independiente del monorepo `olympus-core`: no importa `atlas` ni ningún otro módulo.

## Instalación

```sh
go get github.com/svallejo-dev/olympus-core/hermes-http@latest
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

u, err := hermes.Get[User](ctx, c, "/users/42") // GET https://api.example.com/v1/users/42
if statusErr, ok := errors.AsType[*hermes.StatusError](err); ok && statusErr.StatusCode == http.StatusNotFound {
	return ErrUserNotFound
}
if err != nil {
	return fmt.Errorf("get user: %w", err)
}

created, err := hermes.Post[User](ctx, c, "/users", User{Name: "Ada"},
	hermes.IdempotencyKey(requestID), // un POST solo se reintenta si se sabe idempotente
	hermes.Query(url.Values{"notify": {"true"}}),
)
```

- `Get`, `Post` y los demás esperan JSON y devuelven `*hermes.StatusError` para cualquier respuesta no-2xx, con
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

## Tres niveles de control

| Quieres | Usa | Quién cierra el body |
|---|---|---|
| El resultado JSON | `Get`, `Post`, `Put`, `Patch`, `Delete`, o `Send` para otro método | hermes |
| El resultado JSON y las cabeceras de la respuesta | `Do` y luego `Decode` | hermes (`Decode`) |
| Streaming u otro formato | `Do` y leer `resp.Body` | Tú |

Los dos primeros comprueban el status, limitan el tamaño, decodifican, y cierran el body para liberar la
conexión y el hueco de `WithMaxConcurrent`. Para los casos en que necesitas la respuesta, como paginar:

```go
req, err := c.NewRequest(ctx, http.MethodGet, "/users", nil)
if err != nil {
	return err
}
resp, err := c.Do(req)
if err != nil {
	return err
}
users, err := hermes.Decode[[]User](c, resp)
if err != nil {
	return err
}
next := resp.Header.Get("Link")
```

Si usas `Do` sin `Decode`, cierra `resp.Body` siempre, o esa llamada retiene su hueco de
`WithMaxConcurrent`.

### Dónde va hermes en tu aplicación

hermes decodifica la respuesta; convertirla a tu dominio es trabajo de tu código. `T` debería ser el
contrato de la API externa, y hermes debería conocerse solo en el adaptador que la llama:

```go
// catalog/client.go
type productResponse struct {
	ID         string `json:"id"`
	PriceCents int64  `json:"price_cents"`
}

func (c *Client) FindProduct(ctx context.Context, id string) (order.Product, error) {
	res, err := hermes.Get[productResponse](ctx, c.http, "/products/"+id)
	if statusErr, ok := errors.AsType[*hermes.StatusError](err); ok && statusErr.StatusCode == http.StatusNotFound {
		return order.Product{}, order.ErrProductNotFound
	}
	if err != nil {
		return order.Product{}, fmt.Errorf("find product %s: %w", id, err)
	}
	return order.NewProduct(res.ID, res.PriceCents)
}
```

Si la API externa renombra un campo, solo cambia este archivo. Y como `Get` es una función genérica, no
puede formar parte de una interfaz: la interfaz que uses en tus tests debe ser la de tu adaptador, como
`FindProduct`, no la de hermes.

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

Los campos a cero de `RetryPolicy` y `BreakerPolicy` conservan el valor actual, que de entrada es el
de esta tabla, así que solo escribes los que cambias. Bulkhead y rate limit cubren todas las peticiones y todos los hosts del
`Client` y de los [endpoints](#configuración-por-endpoint) que los compartan, y la espera respeta el `ctx`. El bulkhead cuenta llamadas: una llamada ocupa su hueco
durante todos sus reintentos hasta que se cierra el body. El rate limit marca el ritmo de los
**intentos**: cada reintento espera su propio token, y si el token llegaría después del deadline
falla al instante con un error que envuelve `context.DeadlineExceeded`, sin reintentar.

El timeout total es el `Timeout` del `http.Client` interno (y del que devuelve `HTTPClient()`):
cubre la llamada lógica entera, con reintentos, redirecciones y lectura del body. El timeout de
intento cuelga del contexto de la petición, así que nunca dura más que el deadline restante.

## Configuración por endpoint

No todos los endpoints merecen lo mismo. `c.Endpoint` deriva un `Client` con la misma base URL y
configuración, cambiando solo lo que le pases. Acepta las mismas opciones que `hermes.New`:

```go
c, err := hermes.New("https://api.example.com/v1", hermes.WithBearerToken(tokens.Get))
if err != nil {
	return err
}

// Camino crítico: más intentos, y un breaker propio al que no le afectan los fallos del resto.
payments, err := c.Endpoint("payments",
	hermes.WithRetry(hermes.RetryPolicy{MaxAttempts: 5, MaxBackoff: 5 * time.Second}),
	hermes.WithCircuitBreaker(hermes.BreakerPolicy{ConsecutiveFailures: 10, OpenTimeout: 5 * time.Second}),
	hermes.WithAttemptTimeout(3*time.Second),
)
if err != nil {
	return err
}

// Secundario y lento: sin reintentos, con más tiempo y limitado para no saturar la API.
reports, err := c.Endpoint("reports",
	hermes.WithoutRetry(),
	hermes.WithTimeout(2*time.Minute),
	hermes.WithAttemptTimeout(time.Minute),
	hermes.WithRateLimit(5, 1),
)
if err != nil {
	return err
}

charge, err := hermes.Post[Charge](ctx, payments, "/payments", in, hermes.IdempotencyKey(key))
report, err := hermes.Get[Report](ctx, reports, "/reports/"+id)
```

Lo normal es crearlos al arrancar y guardarlos en el adaptador de esa API, de modo que cada método
use el suyo. Un mismo `Endpoint` puede servir a varias rutas. También funciona con `Do`, con `Decode`
y con `HTTPClient()`.

| Ajuste | Si no lo pasas | Si lo pasas |
|---|---|---|
| Timeouts, cabeceras, token, tamaño máximo | Se copia del padre | Sustituye la copia |
| `WithRetry` | Se copia del padre | Cambia los campos que escribas; el resto se hereda del padre |
| `WithCircuitBreaker` | **Comparte el breaker** del padre | **Breaker propio**, independiente del padre; los campos que no escribas se heredan |
| Rate limit, bulkhead | **Comparte** los tokens y los huecos del padre | **Límite propio**, independiente del padre |
| Conexiones | Comparte el pool del padre | Con `WithTransport`, el transporte que indiques |

Para quitar uno de los compartidos: `WithoutCircuitBreaker()`, `WithoutRateLimit()` o
`WithoutMaxConcurrent()`.

Con `WithRetry` y `WithCircuitBreaker` solo escribes lo que cambia. Si el padre tiene
`MaxBackoff: 10 * time.Second`, `RetryPolicy{MaxAttempts: 6}` conserva esos 10 s, y también su
`IdempotencyHeader`, `IsIdempotent` y `ShouldRetry`. Un campo a cero no puede apagar lo heredado. Para
partir de los valores por defecto de hermes, pon el `Without` antes:

```go
strict, err := c.Endpoint("strict", hermes.WithoutRetry(), hermes.WithRetry(hermes.RetryPolicy{MaxAttempts: 2}))
```

Dos cosas a tener en cuenta:

- **Aislar un endpoint exige pasarle su breaker.** Sin `WithCircuitBreaker`, los fallos de
  cualquier endpoint que comparta breaker pueden abrirlo para todos. Si el host cae entero, cada
  breaker propio tiene que abrirse por su cuenta.
- **Un rate limit propio no consume el del padre.** Si la API impone una cuota global, la suma de
  los límites de tus endpoints no debe superarla.

El nombre aparece como `hermes.endpoint` en la telemetría, así que debe ser fijo y único: un
nombre con IDs dispara la cardinalidad de las métricas, y dos breakers propios con el mismo nombre
pisan su serie de estado.

## Cuándo se reintenta

Se reintenta una petición cuando se cumple **todo** esto:

1. Quedan intentos y el `ctx` del llamador sigue vivo (cancelar corta en seco, sin reintentar).
2. El método es idempotente (`GET`, `HEAD`, `OPTIONS`, `PUT`, `DELETE`, `TRACE`), o la petición
   cumple alguna regla de [idempotencia](#post-y-patch-idempotentes).
3. El body se puede rebobinar: sin body, o con `req.GetBody` (lo pone `http.NewRequest` para
   `bytes.Reader`, `bytes.Buffer` y `strings.Reader`; `Send` y sus atajos siempre lo cumplen).
4. El resultado es reintentable: error de transporte (incluido un timeout de intento) o estado
   429, 502, 503 o 504. `RetryPolicy.ShouldRetry` sustituye esta clasificación.

Nunca se reintenta, diga lo que diga `ShouldRetry`, un rechazo del circuit breaker, un fallo de
`WithBearerToken` ni una espera del rate limit que no cabe en el deadline. Si la respuesta trae
`Retry-After` (segundos o fecha HTTP) se espera eso en lugar del backoff; si pide más de
`RetryPolicy.MaxRetryAfter` (30 s por defecto) o la espera no cabe en el deadline restante, se
para y se devuelve la última respuesta. Los bodies de las respuestas descartadas se drenan y
cierran para reutilizar la conexión.

## POST y PATCH idempotentes

Un `POST` o `PATCH` no se reintenta salvo que le digas a hermes que es seguro, porque repetirlo
podría duplicar la operación. Cada API marca la idempotencia a su manera, así que hay varias formas:

| La API deduplica por... | Configuración |
|---|---|
| La cabecera `Idempotency-Key` | `hermes.IdempotencyKey(clave)` en la llamada |
| Otra cabecera | `RetryPolicy{IdempotencyHeader: "X-Request-Id"}` y `hermes.Header("X-Request-Id", clave)` en la llamada. Sustituye a `Idempotency-Key` |
| Una clave en el body o en la query, solo en algunas llamadas | `hermes.Idempotent()` en la llamada |
| Una regla por ruta o por query | `RetryPolicy{IsIdempotent: func(r *http.Request) bool {...}}` |
| Todo lo que expone ese cliente es seguro | `RetryPolicy{RetryNonIdempotent: true}` |

```go
payments, err := hermes.New(baseURL, hermes.WithRetry(hermes.RetryPolicy{
	IsIdempotent: func(r *http.Request) bool {
		return r.URL.Path == "/v1/payments" || r.URL.Query().Has("requestId")
	},
}))

// La clave va en el body: cada reintento reenvía el mismo body, y por tanto la misma clave.
charge, err := hermes.Post[Charge](ctx, payments, "/v1/payments", Payment{Key: key, Amount: 100})
```

hermes no busca la clave ni comprueba que exista: solo decide si puede repetir la petición. Las
cabeceras que pone `WithHeader` no cuentan para `IdempotencyHeader`, porque se añaden después de
esa decisión. `hermes.Idempotent()` solo se aplica a `Send` y sus atajos; con `Do` o `HTTPClient()`
usa `IsIdempotent`, que no debe consumir el body.

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
sobrevive a un reinicio. Crea un `Client` por API de destino, deriva sus [endpoints](#configuración-por-endpoint) y reutilízalos.

## Telemetría

Cada llamada lógica produce **un** span de cliente (otelhttp), con la cabecera `traceparent`
inyectada una vez y compartida por todos los intentos; cada reintento añade un evento
`hermes.retry` al span. Métricas, además de las `http.client.*` de otelhttp:

| Métrica | Tipo | Atributos |
|---|---|---|
| `hermes.client.retries` | contador | `server.address`, `server.port`, `hermes.retry.reason` (`503`, `timeout`, `transport_error`…) |
| `hermes.client.circuit_breaker.state` | gauge (0 cerrado, 1 semiabierto, 2 abierto) | `server.address`, `server.port` |
| `hermes.client.circuit_breaker.rejections` | contador | `server.address`, `server.port` |

Cuando una llamada falla sin respuesta, `error.type` en `http.client.request.duration` nombra el
motivo: `net.OpError` para un fallo de red, y tipos propios de hermes para los demás
(`...hermes-http.circuitOpenError`, `attemptTimeoutError`, `rateLimitError`, `concurrencyError`,
`retryWaitError`).

Un `Client` creado con `c.Endpoint` añade `hermes.endpoint` a `hermes.client.retries`, a
`http.client.request.duration` y a su span, así que puedes separar la latencia y los fallos del
camino crítico. Las métricas del breaker llevan el nombre del dueño del breaker: el del endpoint
si tiene uno propio, y ninguno si comparte el del `Client` raíz.

gobreaker cambia de estado de forma perezosa: el gauge pasa de abierto a semiabierto cuando llega
la siguiente petición tras `OpenTimeout`, no en el instante en que vence. Un host sin tráfico se
queda en 2 aunque su breaker ya admitiría una prueba.

Por defecto se usan los providers y el propagador **globales** de OpenTelemetry. Los globales
delegan, así que da igual si se registran antes o después de `hermes.New`. Por eso, en un
servicio arrancado con `atlas`, que registra los globales al iniciar, las métricas de hermes
aparecen en su `/metrics` y los spans se encadenan con los del servidor sin que ningún módulo
importe al otro. Para inyectarlos explícitamente: `WithMeterProvider`, `WithTracerProvider` y
`WithPropagators`. Los cambios de estado del breaker se loguean en `Warn` y los reintentos en
`Debug` con `slog.Default()` o el logger de `WithLogger`. Con atlas y el logger por defecto, los
reintentos llevan `request_id` y `trace_id`; los cambios del breaker no, porque afectan al host y no a
una petición.

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
