# elagoht/otel

A collage plugin for OpenTelemetry tracing: collage's own spans become
OpenTelemetry spans, and a request continues the trace its caller started, so every
span of a request — across services — is one trace.

```go
t := otel.NewTracer(provider.Tracer("example.com/site"))

app, err := collage.New(&collage.Config{
	Observability: collage.ObservabilityConfig{Tracer: t},
	Plugins:       []collage.Plugin{t},
})
```

Requires collage v0.50.0 or later.

## Both lines

The one value is two things, and it is handed over as both:

- **As `Config.Observability.Tracer`** it turns the spans collage opens —
  `collage.http` around a request, `collage.render` around a page, `collage.fragment`
  around each fragment — into OpenTelemetry spans. `SetAttribute` becomes a string
  attribute; `RecordError` records the error on the span and sets its status to
  `Error`.
- **As a plugin** it starts every request's **server span**: the trace context in
  the request's headers is read with the propagator, and the server span's parent
  is the caller's.

Each works alone. The tracer alone traces what collage does, a new trace per
request. The plugin alone gives every request one server span continuing the
caller's trace, and nothing inside it.

## One trace per request

The plugin is a `collage.RequestHook`: its `OnRequest` runs before collage opens
`collage.http`, before middleware and before routing. It reads the caller's trace
context and starts the server span, and collage serves the request under the
server span's context — so `collage.http` is its child, and the render and every
fragment nest under that:

```
GET /blog/{slug}          server   ← child of the caller's span
└─ collage.http
   └─ collage.render
      ├─ collage.fragment
      └─ collage.fragment
```

A request the application's own middleware answers — a `401` from an auth check —
is inside the server span too, because middleware runs after `OnRequest`.

Once the response is written, collage hands the plugin the status, and the span is
named for the route the request resolved to, from `collage.RouteInfo`:
`GET /blog/{slug}`, never `GET /blog/hello`, because a trace store groups by name
and a name per URL is a group per URL. A request that resolved to nothing — a
`404` — keeps the method as its name.

| Route | `http.route` |
| --- | --- |
| a page, a document or an action | the pattern it was registered with, with the locale prefix when the request had one: `/tr/blog/{slug}`, `/feed.xml` |
| a mount or a handler | its prefix: `/static/` |

| Attribute | |
| --- | --- |
| `http.request.method` | `GET` |
| `http.route` | as above |
| `http.response.status_code` | `200` |
| `url.path`, `url.scheme`, `user_agent.original` | from the request |
| `collage.route.kind`, `collage.route.name` | what the request resolved to: `page` and `post`, `document` and `feed` |
| `collage.page` | the page it resolved to |
| `collage.locale` | the locale it resolved to |

`collage.http` keeps what collage sets on it: `http.method`, `http.path`,
`http.status_code`.

A `5xx` sets the span's status to `Error`. A `4xx` does not: for a server it is the
client's mistake.

## Setting up the SDK

The application owns the SDK: the provider, the exporter, the sampler, the
propagator. This package only speaks the API. With the stdout exporter, to see spans
in the terminal:

```go
import (
	gotel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	otel "github.com/Elagoht/collage-otel"
)

exporter, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
if err != nil {
	log.Fatal(err)
}
provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
defer provider.Shutdown(context.Background())

gotel.SetTracerProvider(provider)
gotel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{}, propagation.Baggage{},
))

t := otel.NewTracer(provider.Tracer("example.com/site"))
```

In production swap the exporter for `otlptracehttp` or `otlptracegrpc`, pointed at
a collector.

**Set a propagator.** The global one reads nothing until the application sets it,
and every request then starts a trace of its own. `Options.Propagator` sets one for
this plugin alone.

## Options

```go
otel.New(otel.Options{
	Tracer:     provider.Tracer("example.com/site"),
	Propagator: propagation.TraceContext{},
	Skip:       []string{"/healthz"},
})
```

| Option | Default | |
| --- | --- | --- |
| `Tracer` | the global provider's tracer for `github.com/Elagoht/collage-otel` | Starts every span. Go only |
| `Propagator` | `otel.GetTextMapPropagator()` | Reads the caller's context from the headers. Go only |
| `Skip` (`skip`) | none | Path prefixes that get no server span |

`NewTracer(tracer)` is `New(Options{Tracer: tracer})`. A skipped request's
`collage.http` is still started, as a trace of its own, when this package is
collage's tracer; a health check polled every second is better sampled out in the
SDK than traced at all.

## Configuration

```json
{
  "elagoht/otel": {
    "skip": ["/healthz"]
  }
}
```

A skip prefix that does not begin with `/` stops the application from starting.

## Limitations

- Only string attributes reach a span through `collage.Span.SetAttribute`; that is
  the interface collage calls.

## Changes

### v0.2.8

- Retracts v0.2.6, tagged by mistake on the previous release's code. Use v0.2.7 or later. Nothing else changes.

### v0.2.7

- Requires collage v0.50.0. Plugin configuration is read with `collage.PluginConfig`, since `host.Config` is gone. Nothing else changes.

### v0.2.7

- Requires collage v0.49.0. Tests only: the test site gives its fragments
  typed data with `collage.Load` and `collage.DataHandler`, since
  `WithDataHandler` is gone. The plugin itself is unchanged.

### v0.2.3

- Tests only: the test site registers its `"tr"` path only when it supports
  `"tr"`, which collage v0.35.0 requires. The plugin itself is unchanged.

### v0.2.2

- `collage.json`: the plugin described to editors — its template functions,
  snippets and configuration schema — for the Collage Snippets & Highlighter
  extension and any tool reading it.

### v0.2.1

- `http.route` comes from `collage.RouteInfo` (collage v0.26.0): a document and an
  action are named for the pattern they were registered with, like a page, rather
  than their registered name. The `OnPageResolved` hook is gone.
- Requires collage v0.26.0.

### v0.2.0

- The server span is started in `OnRequest`, collage v0.25.0's `RequestHook`,
  before collage opens `collage.http`. `collage.http` is now the server span's
  child rather than being held back and started as the server span, and the
  plugin no longer adds middleware or wraps the response writer.
- A request the application's middleware answers is inside the caller's trace.
- `http.route` comes from `collage.RouteOf`: a document, an action, a mount and a
  handler are named for their route too, where only a page was before. New
  attributes `collage.route.kind` and `collage.route.name`.
- `collage.http`'s own attributes — `http.method`, `http.path`, `http.status_code`
  — stay on `collage.http` rather than on the server span.
- Requires collage v0.25.0.
