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

Requires collage v0.23.0 or later.

## Both lines

The one value is two things, and it is handed over as both:

- **As `Config.Observability.Tracer`** it turns the spans collage opens —
  `collage.http` around a request, `collage.render` around a page, `collage.fragment`
  around each fragment — into OpenTelemetry spans. `SetAttribute` becomes a string
  attribute; `RecordError` records the error on the span and sets its status to
  `Error`.
- **As a plugin** it wraps every request: the trace context in the request's
  headers is read with the propagator, and the request's span is a **server span**
  whose parent is the caller's.

Each works alone. The tracer alone traces what collage does, a new trace per
request. The plugin alone gives every request one server span continuing the
caller's trace, and nothing inside it.

## One server span per request

collage opens `collage.http` before any middleware runs, and a plugin's middleware
is the first code that can read the request's headers. A span cannot be re-parented
once it has started. So when both halves are installed, `collage.http` is held when
collage asks for it — its start time and the attributes collage gives it are kept —
and started by the middleware, once the caller's context is known, as the server
span, with its original start time. The render and every fragment nest under it:

```
GET /blog/{slug}          server   ← child of the caller's span
└─ collage.render
   ├─ collage.fragment
   └─ collage.fragment
```

A request the application's own middleware answers before the plugin's — a `401`
from an auth check — still gets its span, as a trace of its own.

The span is named for the method, then renamed for the route once the page is
resolved: `GET /blog/{slug}`, never `GET /blog/hello`, because a trace store groups
by name and a name per URL is a group per URL.

| Attribute | |
| --- | --- |
| `http.request.method` | `GET` |
| `http.route` | the page's pattern, with the locale prefix when the request had one |
| `http.response.status_code` | `200` |
| `url.path`, `url.scheme`, `user_agent.original` | from the request |
| `collage.page`, `collage.locale` | the page it resolved to |
| `http.method`, `http.path`, `http.status_code` | what collage sets on `collage.http` itself |

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
| `Skip` (`skip`) | none | Path prefixes the middleware does not trace |

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

- A **document** — a sitemap, a feed — has no `PageResolved` hook, so its span keeps
  the method as its name and has no `http.route`.
- collage opens `collage.http` before middleware, so continuing the caller's trace
  depends on holding that span until the middleware runs. Were collage to start its
  request span from a context a plugin could shape first, or read the trace context
  itself, the holding would go.
- A request the application's own middleware answers before this plugin's is not
  linked to its caller's trace: its headers were never read.
- Only string attributes reach a span through `collage.Span.SetAttribute`; that is
  the interface collage calls.
