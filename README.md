# elagoht/otel

A collage plugin for OpenTelemetry tracing: collage's own spans become
OpenTelemetry spans, and a request continues the trace its caller started, so every
span of a request — across services — is one trace.

```go
t := otel.New(otel.Options{Tracer: provider.Tracer("example.com/site")})

app, err := collage.New(&collage.Config{
	Observability: collage.ObservabilityConfig{Tracer: t},
	Plugins:       []collage.Plugin{t},
})
```

Requires collage v0.61.0 or later.

## Both lines

The one value is two things, and it is handed over as both:

- **As `Config.Observability.Tracer`** it turns the spans collage opens —
  `collage.http` around a request, `collage.render` around a page, `collage.fragment`
  around each fragment — into OpenTelemetry spans. `SetAttribute` becomes a string
  attribute; `RecordError` records the error on the span and sets its status to
  `Error`.
- **As a plugin** it starts every request's **server span**: the trace context in
  the request's headers is read with the propagator, and the server span's parent
  is the caller's — when the caller is trusted (see
  [Whose trace is continued](#whose-trace-is-continued)).

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

The method is bounded the same way. net/http accepts any token as a method, so a
method outside RFC 9110's (`GET`, `HEAD`, `POST`, `PUT`, `DELETE`, `CONNECT`,
`OPTIONS`, `TRACE`) and `PATCH` is `_OTHER` in `http.request.method`, with the
method as sent in `http.request.method_original`, and `HTTP` in the span's name —
`HTTP /blog/{slug}`, or `HTTP` alone — as OpenTelemetry's HTTP conventions name
it. A client inventing methods does not invent span names.

| Route | `http.route` |
| --- | --- |
| a page, a document or an action | the pattern it was registered with, with the locale prefix when the request had one: `/tr/blog/{slug}`, `/feed.xml` |
| a mount or a handler | its prefix: `/static/` |

| Attribute | |
| --- | --- |
| `http.request.method` | `GET`; `_OTHER` for a method outside RFC 9110's and `PATCH` |
| `http.request.method_original` | the method as sent, only when `http.request.method` is `_OTHER` |
| `http.route` | as above |
| `http.response.status_code` | `200` |
| `url.path`, `url.scheme`, `user_agent.original` | from the request; the scheme is `collage.IsHTTPS`, so `https` behind a trusted TLS-terminating proxy |
| `collage.route.kind`, `collage.route.name` | what the request resolved to: `page` and `post`, `document` and `feed` |
| `collage.page` | the page it resolved to |
| `collage.locale` | the locale it resolved to |

`collage.http` keeps what collage sets on it: `http.method`, `http.path`,
`http.status_code`.

A `5xx` sets the span's status to `Error`. A `4xx` does not: for a server it is the
client's mistake.

## Whose trace is continued

A `traceparent` header is a claim, and anyone can send one. Continued from anyone,
it lets an internet client decide what is sampled — the usual
`ParentBased(...)` sampler follows the caller's sampled flag, so one header makes
every request of theirs exported — and put the site's spans in a trace id of the
client's choosing. Its `baggage`, read by the same propagator, would reach the
application's code.

So by default the trace context is continued only on a request whose connection
comes from one of the application's `Config.Server.TrustedProxies` — the proxies
`collage.ClientIP` believes. With none listed, the default, no one's is. Any other
request's trace context is not discarded: its server span is the root of a new
trace with a **link** to the caller's span, so a trace store can still follow it,
and the sampler decides about it afresh. Its baggage is dropped.

`trustIncoming` changes that:

| `trustIncoming` | continued |
| --- | --- |
| `""` (default) | from a trusted proxy only |
| `"always"` | from anyone: a service that only other services reach, with no proxy in front |
| `"never"` | from no one: every request starts a trace of its own, linked to the caller's |

A trusted proxy passes on the `traceparent` its own client sent unless it is told
not to. A CDN or a load balancer in front of the public site usually forwards it
as it came, so "through a trusted proxy" does not by itself mean "from a caller
you trust": strip `traceparent`, `tracestate` and `baggage` at the proxy, or set
`"never"`, unless the proxy is the start of the trace.

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

t := otel.New(otel.Options{Tracer: provider.Tracer("example.com/site")})
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
| `TrustIncoming` (`trustIncoming`) | `""`: only from `Server.TrustedProxies` | Whose trace context is continued: `""`, `"always"` or `"never"`; see [Whose trace is continued](#whose-trace-is-continued) |
| `Skip` (`skip`) | none | Path prefixes that get no server span, matched at a segment boundary with or without the trailing `/` (`"/blog"` and `"/blog/"` both skip `/blog` and `/blog/x`, not `/blogger`) |

A skipped request's
`collage.http` is still started, as a trace of its own, when this package is
collage's tracer; a health check polled every second is better sampled out in the
SDK than traced at all.

## Configuration

```json
{
  "elagoht/otel": {
    "skip": ["/healthz"],
    "trustIncoming": ""
  }
}
```

A skip prefix that does not begin with `/`, and a `trustIncoming` other than `""`,
`"always"` and `"never"`, stop the application from starting.

## Limitations

- Only string attributes reach a span through `collage.Span.SetAttribute`; that is
  the interface collage calls.

## Changes

### Changed in v0.4.0

- **Behaviour change: a client's trace context is no longer continued by
  default.** It is continued only on a request through one of
  `Server.TrustedProxies`; any other request starts a new trace that links to the
  caller's span, and its baggage is dropped. Set `"trustIncoming": "always"` for
  the old behaviour — a service only other services reach — or `"never"` to
  continue no one's. New option `TrustIncoming` (`trustIncoming`).
- A method outside RFC 9110's and `PATCH` is `_OTHER` in `http.request.method`,
  with the method as sent in `http.request.method_original`, and `HTTP` in the
  server span's name, where it used to be the method as sent.

### Changed in v0.3.0

| Was | Now |
| --- | --- |
| `otel.NewTracer(tracer)` | `otel.New(otel.Options{Tracer: tracer})` |
| `otel.Span` (exported adapter) | unexported; read the OpenTelemetry span with `trace.SpanFromContext(ctx)` |

- `url.scheme` follows `collage.IsHTTPS`: `https` behind a trusted proxy that says
  so, where it was `http` for every proxied request.
- A `skip` prefix matches with or without its trailing slash, at a segment
  boundary: `"/blog/"` now skips `/blog` itself, and `"/blog"` no longer skips
  `/blogger`.
- Requires collage v0.61.0.

### v0.2.10

- Requires collage v0.60.0 (sealed public API). No behaviour change.

### v0.2.9

- Requires collage v0.52.0. A static build's header capture (`collage.IsCapture`) is not traced: it gets no server span, and `StartSpan` hands collage a span that records nothing for it, so `collage.http`, `collage.render` and `collage.fragment` are not exported for the build's own requests. The plugin never set a response header, and still sets none.

### v0.2.8

- Retracts v0.2.6, tagged by mistake on the previous release's code. Use v0.2.7 or later. Nothing else changes.

### v0.2.7

- Requires collage v0.50.0. Plugin configuration is read with `collage.PluginConfig`, since `host.Config` is gone. Nothing else changes.

### v0.2.5

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
