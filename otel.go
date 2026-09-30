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
// status and named for the route the request resolved to, from collage.RouteInfo.
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
func (p *Plugin) Version() string                { return "0.2.4" }
func (p *Plugin) Shutdown(context.Context) error { return nil }

var (
	_ collage.Plugin      = (*Plugin)(nil)
	_ collage.Tracer      = (*Plugin)(nil)
	_ collage.RequestHook = (*Plugin)(nil)
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
	method, path := r.Method, r.URL.Path
	return ctx, func(status int) {
		if route, attrs := route(ctx, path); route != "" {
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
// say so: the pattern the page, document or action was registered with, with the
// locale prefix when the request's path had one; a mount's or a handler's
// prefix. An unresolved request — a 404 — has none.
func route(ctx context.Context, path string) (string, []attribute.KeyValue) {
	info := collage.RouteInfo(ctx)
	if info.Kind == "" {
		return "", nil
	}
	pattern := info.Pattern
	if pattern == "" {
		pattern = info.Name
	}
	// The locale prefix the router stripped is part of the route that matched.
	if prefix := "/" + info.Locale; info.Locale != "" && (path == prefix || strings.HasPrefix(path, prefix+"/")) {
		pattern = strings.TrimSuffix(prefix+pattern, "/")
		if pattern == "" {
			pattern = "/"
		}
	}
	attrs := []attribute.KeyValue{
		attribute.String("collage.route.kind", info.Kind),
		attribute.String("collage.route.name", info.Name),
		attribute.String("http.route", pattern),
	}
	if info.Kind == "page" {
		attrs = append(attrs, attribute.String("collage.page", info.Name))
	}
	if info.Locale != "" {
		attrs = append(attrs, attribute.String("collage.locale", info.Locale))
	}
	return pattern, attrs
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}
