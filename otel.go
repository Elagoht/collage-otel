// Package otel is a collage plugin for OpenTelemetry tracing.
//
//	t := otel.NewTracer(provider.Tracer("example.com/site"))
//	app, err := collage.New(&collage.Config{
//		Observability: collage.ObservabilityConfig{Tracer: t},
//		Plugins:       []collage.Plugin{t},
//	})
//
// The value is two things. As collage's Tracer it turns the framework's own spans —
// collage.http, collage.render, collage.fragment — into OpenTelemetry spans. As a
// plugin it continues the trace a request arrives with: the trace context in its
// headers is read with the propagator, and the request gets a server span that is a
// child of the caller's, so a request that crossed three services is one trace
// rather than three.
//
// # One trace per request
//
// The server span is started in OnRequest, collage's RequestHook, which runs before
// collage opens its own request span. So collage.http starts as the server span's
// child, and every span collage opens afterwards — the render, each fragment —
// nests under that. When the response is written the server span is told the
// status and named for the route the request resolved to, from collage.RouteOf.
//
// The application sets up the SDK: the tracer provider, the exporter, the
// propagator. This package only speaks the API.
package otel

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/Elagoht/collage/pkg/collage"
	gotel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Name is the plugin's name, and the key its configuration is found under.
const Name = "elagoht/otel"

// ScopeName is the instrumentation scope of the tracer used when none is given.
const ScopeName = "github.com/Elagoht/collage-otel"

// Options configures the plugin.
type Options struct {
	// Tracer starts the spans. Unset, it is the global tracer provider's tracer
	// for ScopeName, so an application that has called otel.SetTracerProvider
	// need pass nothing. It can only be set from Go.
	Tracer trace.Tracer `json:"-"`
	// Propagator reads the trace context from a request's headers. Unset, it is
	// the global one, otel.GetTextMapPropagator — which reads nothing until the
	// application sets one. It can only be set from Go.
	Propagator propagation.TextMapPropagator `json:"-"`
	// Skip are path prefixes whose requests get no server span: a health check a
	// load balancer polls every second is noise in a trace store. Their
	// collage.http span is still started, as a trace of its own, when this
	// package is collage's Tracer.
	Skip []string `json:"skip"`
}

// Plugin is collage's Tracer and the request hook that continues a request's
// trace.
type Plugin struct {
	opts   Options
	tracer trace.Tracer
}

// New returns the plugin with opts as its starting point, which the application's
// own configuration is then decoded over.
func New(opts Options) *Plugin {
	p := &Plugin{opts: opts, tracer: opts.Tracer}
	if p.tracer == nil {
		p.tracer = gotel.Tracer(ScopeName)
	}
	return p
}

// NewTracer returns the plugin with tracer starting its spans: hand the same value
// to Config.Observability.Tracer and to Config.Plugins.
func NewTracer(tracer trace.Tracer) *Plugin { return New(Options{Tracer: tracer}) }

func (p *Plugin) Name() string                   { return Name }
func (p *Plugin) Version() string                { return "0.2.0" }
func (p *Plugin) Shutdown(context.Context) error { return nil }

var (
	_ collage.Plugin           = (*Plugin)(nil)
	_ collage.Tracer           = (*Plugin)(nil)
	_ collage.RequestHook      = (*Plugin)(nil)
	_ collage.PageResolvedHook = (*Plugin)(nil)
)

// Init reads the configuration.
func (p *Plugin) Init(_ context.Context, host collage.Host) error {
	if err := host.Config(&p.opts); err != nil {
		return err
	}
	for _, prefix := range p.opts.Skip {
		if !strings.HasPrefix(prefix, "/") {
			return fmt.Errorf("otel: skip prefix %q must begin with /", prefix)
		}
	}
	return nil
}

func (p *Plugin) propagator() propagation.TextMapPropagator {
	if p.opts.Propagator != nil {
		return p.opts.Propagator
	}
	return gotel.GetTextMapPropagator()
}

// StartSpan starts an OpenTelemetry span as a child of any span in ctx — for
// collage.http, the server span OnRequest started.
func (p *Plugin) StartSpan(ctx context.Context, name string) (context.Context, collage.Span) {
	ctx, span := p.tracer.Start(ctx, name)
	return ctx, Span{span}
}

// Span adapts an OpenTelemetry span to collage.Span.
type Span struct{ trace.Span }

// SetAttribute sets a string attribute.
func (s Span) SetAttribute(key, value string) { s.SetAttributes(attribute.String(key, value)) }

// RecordError records err on the span and marks it failed. A nil err does nothing.
func (s Span) RecordError(err error) {
	if err == nil {
		return
	}
	s.Span.RecordError(err)
	s.SetStatus(codes.Error, err.Error())
}

// End ends the span.
func (s Span) End() { s.Span.End() }

// requestKey carries a request's state from OnRequest to OnPageResolved, which is
// handed the request's context but not the request.
type requestKey struct{}

// request is what the server span learns while the request is served. It is
// written and read on the request's own goroutine, as collage serves a request.
type request struct {
	// pattern and locale are the page's, when the request resolved to one:
	// collage.RouteOf names the page, not the URL pattern it matched.
	pattern string
	locale  string
}

// OnRequest starts the request's server span, as a child of the caller's span
// when the headers carry one, and returns its context: collage opens its own
// request span under it. The function it returns ends the span once the response
// is written, named for the route the request resolved to.
func (p *Plugin) OnRequest(r *http.Request) (context.Context, func(status int)) {
	for _, prefix := range p.opts.Skip {
		if strings.HasPrefix(r.URL.Path, prefix) {
			return r.Context(), nil
		}
	}
	ctx := p.propagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	// Named for the method until the route is known: a span named for its path
	// would be a new name for every URL, which is what trace stores group by.
	ctx, span := p.tracer.Start(ctx, r.Method,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("url.path", r.URL.Path),
			attribute.String("url.scheme", scheme(r)),
			attribute.String("user_agent.original", r.UserAgent()),
		),
	)
	state := &request{}
	ctx = context.WithValue(ctx, requestKey{}, state)
	method := r.Method
	return ctx, func(status int) {
		if route, attrs := state.route(ctx); route != "" {
			span.SetName(method + " " + route)
			span.SetAttributes(attrs...)
		}
		span.SetAttributes(attribute.Int("http.response.status_code", status))
		// A 4xx is the client's mistake, not the server's: for a server span
		// only a 5xx is an error.
		if status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
		span.End()
	}
}

// route is what the request resolved to, as http.route, and the attributes that
// say so. A page is its URL pattern, with the locale prefix when the request had
// one; a mount or a handler is its prefix; a document or an action is its
// registered name, because a plugin is not told a document's pattern. An
// unresolved request — a 404 — has none.
func (s *request) route(ctx context.Context) (string, []attribute.KeyValue) {
	kind, name := collage.RouteOf(ctx)
	if kind == "" {
		return "", nil
	}
	route := name
	attrs := []attribute.KeyValue{
		attribute.String("collage.route.kind", kind),
		attribute.String("collage.route.name", name),
	}
	if kind == "page" {
		attrs = append(attrs, attribute.String("collage.page", name))
		if s.pattern != "" {
			route = s.pattern
			attrs = append(attrs, attribute.String("collage.locale", s.locale))
		}
	}
	return route, append(attrs, attribute.String("http.route", route))
}

// OnPageResolved records the pattern the page matched, which RouteOf does not
// carry. It fires for a page served from the cache too.
func (p *Plugin) OnPageResolved(ctx context.Context, ev *collage.PageResolvedEvent) error {
	state, ok := ctx.Value(requestKey{}).(*request)
	if !ok || ev.Page == nil {
		return nil
	}
	pattern, found := ev.Page.Paths[ev.Locale]
	if !found {
		return nil
	}
	// The locale prefix the router stripped is part of the route that matched.
	if prefix := "/" + ev.Locale; ev.Locale != "" && (ev.Path == prefix || strings.HasPrefix(ev.Path, prefix+"/")) {
		pattern = strings.TrimSuffix(prefix+pattern, "/")
		if pattern == "" {
			pattern = "/"
		}
	}
	state.pattern, state.locale = pattern, ev.Locale
	return nil
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}
