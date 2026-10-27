// Package otel is a collage plugin for OpenTelemetry tracing.
//
//	t := otel.New(otel.Options{Tracer: provider.Tracer("example.com/site")})
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
// rather than three. By default only a request through one of the application's
// Server.TrustedProxies is continued; any other starts a new trace linked to the
// caller's (Options.TrustIncoming).
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
	"go.opentelemetry.io/otel/trace/noop"
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
	// load balancer polls every second is noise in a trace store. A prefix
	// matches at a segment boundary, with or without its trailing slash: "/blog"
	// and "/blog/" both skip /blog and /blog/x, and neither /blogger. Their
	// collage.http span is still started, as a trace of its own, when this
	// package is collage's Tracer.
	Skip []string `json:"skip"`
	// TrustIncoming says whose trace context is continued. "" (the default)
	// continues it only on a request that came through one of the application's
	// Server.TrustedProxies — so with none listed, no one's; "always" continues
	// anyone's, for a service only other services reach; "never" continues no
	// one's. A trace context that is not continued is recorded as a link on a
	// new trace, and its baggage is dropped: a client must not decide what is
	// sampled, nor plant the site's spans in a trace of its choosing.
	TrustIncoming Trust `json:"trustIncoming"`
}

// Trust says whose incoming trace context is continued. In configuration it is
// "", "always" or "never".
type Trust string

const (
	// TrustProxies continues the trace context of a request that came through
	// one of Server.TrustedProxies, and no other. It is the default.
	TrustProxies Trust = ""
	// TrustAlways continues every request's trace context.
	TrustAlways Trust = "always"
	// TrustNever continues no request's trace context.
	TrustNever Trust = "never"
)

// Plugin is collage's Tracer and the request hook that continues a request's
// trace.
type Plugin struct {
	opts   Options
	tracer trace.Tracer
}

// New returns the plugin with opts as its starting point, which the application's
// own configuration is then decoded over. Hand the same value to
// Config.Observability.Tracer and to Config.Plugins.
func New(opts Options) *Plugin {
	p := &Plugin{opts: opts, tracer: opts.Tracer}
	if p.tracer == nil {
		p.tracer = gotel.Tracer(ScopeName)
	}
	return p
}

func (p *Plugin) Name() string                   { return Name }
func (p *Plugin) Version() string                { return "0.4.0" }
func (p *Plugin) Shutdown(context.Context) error { return nil }

var (
	_ collage.Plugin      = (*Plugin)(nil)
	_ collage.Tracer      = (*Plugin)(nil)
	_ collage.RequestHook = (*Plugin)(nil)
)

// Init reads the configuration.
func (p *Plugin) Init(_ context.Context, host collage.Host) error {
	cfg, err := collage.PluginConfig(host, p.opts)
	if err != nil {
		return err
	}
	// A copy: the caller's Options keep their slice as written.
	skip := make([]string, 0, len(cfg.Skip))
	for _, prefix := range cfg.Skip {
		if !strings.HasPrefix(prefix, "/") {
			return fmt.Errorf("otel: skip prefix %q must begin with /", prefix)
		}
		skip = append(skip, strings.TrimSuffix(prefix, "/"))
	}
	cfg.Skip = skip
	switch cfg.TrustIncoming {
	case TrustProxies, TrustAlways, TrustNever:
	default:
		return fmt.Errorf("otel: trustIncoming %q must be \"\", \"always\" or \"never\"", cfg.TrustIncoming)
	}
	p.opts = cfg
	return nil
}

// skipped reports whether path is under a Skip prefix, each stored without its
// trailing slash ("/" as "").
func (p *Plugin) skipped(path string) bool {
	for _, prefix := range p.opts.Skip {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func (p *Plugin) propagator() propagation.TextMapPropagator {
	if p.opts.Propagator != nil {
		return p.opts.Propagator
	}
	return gotel.GetTextMapPropagator()
}

// StartSpan starts an OpenTelemetry span as a child of any span in ctx — for
// collage.http, the server span OnRequest started. For a static build's header
// capture (collage.IsCapture) it starts nothing: it returns ctx as it is and a
// span that records nothing.
func (p *Plugin) StartSpan(ctx context.Context, name string) (context.Context, collage.Span) {
	if collage.IsCapture(ctx) {
		return ctx, span{noop.Span{}}
	}
	ctx, s := p.tracer.Start(ctx, name)
	return ctx, span{s}
}

// span adapts an OpenTelemetry span to collage.Span. Code that needs the
// OpenTelemetry span itself reads it with trace.SpanFromContext.
type span struct{ trace.Span }

// SetAttribute sets a string attribute.
func (s span) SetAttribute(key, value string) { s.SetAttributes(attribute.String(key, value)) }

// RecordError records err on the span and marks it failed. A nil err does nothing.
func (s span) RecordError(err error) {
	if err == nil {
		return
	}
	s.Span.RecordError(err)
	s.SetStatus(codes.Error, err.Error())
}

// End ends the span.
func (s span) End() { s.Span.End() }

// OnRequest starts the request's server span, as a child of the caller's span
// when the headers carry one and the caller is trusted (TrustIncoming) — a
// root linked to it otherwise — and returns its context: collage opens its own
// request span under it. The function it returns ends the span once the response
// is written, named for the route the request resolved to.
//
// A static build asking for its own files to capture their headers is not
// traced: it gets no server span, and the trace context its headers carry —
// none — is not read.
func (p *Plugin) OnRequest(r *http.Request) (context.Context, func(status int)) {
	if collage.IsCapture(r.Context()) {
		return r.Context(), nil
	}
	if p.skipped(r.URL.Path) {
		return r.Context(), nil
	}
	ctx, start := p.incoming(r)
	method := knownMethod(r.Method)
	attrs := []attribute.KeyValue{
		attribute.String("http.request.method", method),
		attribute.String("url.path", r.URL.Path),
		attribute.String("url.scheme", scheme(r)),
		attribute.String("user_agent.original", r.UserAgent()),
	}
	if method != r.Method {
		attrs = append(attrs, attribute.String("http.request.method_original", r.Method))
	}
	// Named for the method until the route is known: a span named for its path
	// would be a new name for every URL, which is what trace stores group by.
	ctx, span := p.tracer.Start(ctx, spanName(method, ""),
		append(start, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))...)
	path := r.URL.Path
	return ctx, func(status int) {
		if route, attrs := route(ctx, path); route != "" {
			span.SetName(spanName(method, route))
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

// incoming returns the context the server span starts from, and how it starts.
//
// A trusted request's trace context is extracted into the request's context, and
// the server span is the caller's span's child. Any other request's is
// extracted into a context of its own, to read the caller's span from: the
// server span is the root of a new trace that links to it, so the sampler
// decides afresh and the client's trace id names nothing here — and the
// client's baggage, extracted with it, is dropped.
func (p *Plugin) incoming(r *http.Request) (context.Context, []trace.SpanStartOption) {
	if p.trusts(r) {
		return p.propagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header)), nil
	}
	start := []trace.SpanStartOption{trace.WithNewRoot()}
	remote := trace.SpanContextFromContext(p.propagator().Extract(context.Background(), propagation.HeaderCarrier(r.Header)))
	if remote.IsValid() {
		start = append(start, trace.WithLinks(trace.Link{SpanContext: remote}))
	}
	return r.Context(), start
}

// trusts reports whether r's trace context is continued, as TrustIncoming says.
func (p *Plugin) trusts(r *http.Request) bool {
	switch p.opts.TrustIncoming {
	case TrustAlways:
		return true
	case TrustNever:
		return false
	}
	return fromTrustedProxy(r)
}

// fromTrustedProxy reports whether r's connection comes from one of the
// application's Server.TrustedProxies.
//
// collage does not say so directly, but collage.ClientIP does by what it does:
// it reads X-Forwarded-For only from a trusted proxy, and an entry there that is
// not an address leaves the client unknown. So the same connection is asked about
// with an X-Forwarded-For of "unknown": the answer is the connection's own
// address from anyone else, and no address from a trusted proxy. The question is
// asked of a request made for it, so r's own headers are neither read nor changed.
func fromTrustedProxy(r *http.Request) bool {
	probe := (&http.Request{RemoteAddr: r.RemoteAddr, Header: http.Header{}}).WithContext(r.Context())
	if !collage.ClientIP(probe).IsValid() {
		return false
	}
	probe.Header.Set("X-Forwarded-For", "unknown")
	return !collage.ClientIP(probe).IsValid()
}

// knownMethod is method when it is one of RFC 9110's, or PATCH, and "_OTHER"
// otherwise, as OpenTelemetry's HTTP conventions ask. net/http accepts any token
// as a method, so without it a client inventing methods would invent span names.
func knownMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete,
		http.MethodConnect, http.MethodOptions, http.MethodTrace, http.MethodPatch:
		return method
	}
	return "_OTHER"
}

// spanName is the server span's name: the method and the route, the method alone
// before the route is known or when there is none. An unknown method is "HTTP",
// as OpenTelemetry's HTTP conventions name it.
func spanName(method, route string) string {
	if method == "_OTHER" {
		method = "HTTP"
	}
	if route == "" {
		return method
	}
	return method + " " + route
}

// route is what the request resolved to, as http.route, and the attributes that
// say so: the pattern the page, document or action was registered with, with the
// locale prefix when the request's path had one; a mount's or a handler's
// prefix. An unresolved request — a 404 — has none, and neither has one collage
// refused before routing (a dirty path, redirected or answered 404): it carries
// only its kind, so a probe never names a span.
func route(ctx context.Context, path string) (string, []attribute.KeyValue) {
	info := collage.RouteInfo(ctx)
	if info.Kind == "" {
		return "", nil
	}
	if info.Kind == collage.RouteKindRejected {
		return "", []attribute.KeyValue{attribute.String("collage.route.kind", info.Kind)}
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
	if info.Kind == collage.RouteKindPage {
		attrs = append(attrs, attribute.String("collage.page", info.Name))
	}
	if info.Locale != "" {
		attrs = append(attrs, attribute.String("collage.locale", info.Locale))
	}
	return pattern, attrs
}

// scheme is the request's scheme as collage.IsHTTPS decides it: TLS, or a
// trusted proxy's X-Forwarded-Proto.
func scheme(r *http.Request) string {
	if collage.IsHTTPS(r) {
		return "https"
	}
	return "http"
}
