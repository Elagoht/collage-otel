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
// headers is read with the global propagator, and the request's span becomes a
// server span that is a child of the caller's, so a request that crossed three
// services is one trace rather than three.
//
// # One server span per request
//
// collage opens the request's span, collage.http, before any middleware runs, and a
// plugin's middleware is the first code that can read the request's headers. A span
// cannot be re-parented once it has started. So when both halves are installed,
// collage.http is not started when collage asks for it: it is held, with its start
// time and whatever attributes collage gave it, until the middleware has read the
// caller's trace context, and then started as the server span with that parent and
// its original start time. Every span collage opens afterwards — the render, each
// fragment — nests under it. A request that never reaches the middleware, because
// the application's own middleware answered it first, still gets its span, as a
// trace of its own.
//
// The application sets up the SDK: the tracer provider, the exporter, the
// propagator. This package only speaks the API.
package otel

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

// requestSpan is the name collage gives the span it opens around a request.
const requestSpan = "collage.http"

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
	// Skip are path prefixes whose requests are not traced by the middleware: a
	// health check a load balancer polls every second is noise in a trace store.
	// Their collage.http span is still started, as a trace of its own, when this
	// package is collage's Tracer.
	Skip []string `json:"skip"`
}

// Plugin is collage's Tracer and the middleware that continues a request's trace.
type Plugin struct {
	opts   Options
	tracer trace.Tracer
	// installed says Init ran, so the middleware will be there to start the
	// request's span. Until it is, collage.http is started when collage asks,
	// as any other span is.
	installed atomic.Bool
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
func (p *Plugin) Version() string                { return "0.1.0" }
func (p *Plugin) Shutdown(context.Context) error { return nil }

var (
	_ collage.Plugin           = (*Plugin)(nil)
	_ collage.Tracer           = (*Plugin)(nil)
	_ collage.PageResolvedHook = (*Plugin)(nil)
)

// Init reads the configuration and wraps every request.
func (p *Plugin) Init(_ context.Context, host collage.Host) error {
	if err := host.Config(&p.opts); err != nil {
		return err
	}
	for _, prefix := range p.opts.Skip {
		if !strings.HasPrefix(prefix, "/") {
			return fmt.Errorf("otel: skip prefix %q must begin with /", prefix)
		}
	}
	if err := host.Use(p.middleware); err != nil {
		return err
	}
	p.installed.Store(true)
	return nil
}

func (p *Plugin) propagator() propagation.TextMapPropagator {
	if p.opts.Propagator != nil {
		return p.opts.Propagator
	}
	return gotel.GetTextMapPropagator()
}

// StartSpan starts an OpenTelemetry span as a child of any span in ctx. The
// request's own span is held for the middleware instead, when there is one.
func (p *Plugin) StartSpan(ctx context.Context, name string) (context.Context, collage.Span) {
	if name == requestSpan && p.installed.Load() {
		held := &heldSpan{plugin: p, parent: ctx, start: time.Now()}
		return context.WithValue(ctx, heldKey{}, held), held
	}
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

type heldKey struct{}

// heldSpan is collage.http before the middleware has started it. What collage
// tells it meanwhile is kept and applied to the span once there is one.
type heldSpan struct {
	plugin *Plugin
	parent context.Context
	start  time.Time

	mu    sync.Mutex
	span  trace.Span
	attrs []attribute.KeyValue
	errs  []error
	ended bool
}

// begin makes span the one this stands for, handing it what was kept.
func (h *heldSpan) begin(span trace.Span) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.span = span
	span.SetAttributes(h.attrs...)
	for _, err := range h.errs {
		Span{span}.RecordError(err)
	}
	h.attrs, h.errs = nil, nil
}

func (h *heldSpan) SetAttribute(key, value string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.span != nil {
		h.span.SetAttributes(attribute.String(key, value))
		return
	}
	h.attrs = append(h.attrs, attribute.String(key, value))
}

func (h *heldSpan) RecordError(err error) {
	if err == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.span != nil {
		Span{h.span}.RecordError(err)
		return
	}
	h.errs = append(h.errs, err)
}

// End ends the span. One the middleware never started — the request was answered
// before reaching it — is started now, from its original start time, so the
// request is traced all the same.
func (h *heldSpan) End() {
	h.mu.Lock()
	if h.ended {
		h.mu.Unlock()
		return
	}
	h.ended = true
	span := h.span
	h.mu.Unlock()
	if span == nil {
		_, span = h.plugin.tracer.Start(h.parent, requestSpan, trace.WithTimestamp(h.start), trace.WithSpanKind(trace.SpanKindServer))
		h.begin(span)
	}
	span.End()
}

func (p *Plugin) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		held, _ := r.Context().Value(heldKey{}).(*heldSpan)
		for _, prefix := range p.opts.Skip {
			if strings.HasPrefix(r.URL.Path, prefix) {
				next.ServeHTTP(w, r)
				return
			}
		}
		ctx := p.propagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		opts := []trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", r.Method),
				attribute.String("url.path", r.URL.Path),
				attribute.String("url.scheme", scheme(r)),
				attribute.String("user_agent.original", r.UserAgent()),
			),
		}
		if held != nil {
			opts = append(opts, trace.WithTimestamp(held.start))
		}
		// Named for the method until the route is known: a span named for its
		// path would be a new name for every URL, which is what trace stores
		// group by.
		ctx, span := p.tracer.Start(ctx, r.Method, opts...)
		ctx = context.WithValue(ctx, methodKey{}, r.Method)
		if held != nil {
			held.begin(span)
		}
		rec := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(rec, r.WithContext(ctx))

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		span.SetAttributes(attribute.Int("http.response.status_code", status))
		// A 4xx is the client's mistake, not the server's: for a server span
		// only a 5xx is an error.
		if status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
		if held == nil {
			span.End()
		}
	})
}

// OnPageResolved names the request's span for the route it resolved to. It fires
// for a page served from the cache too; a document has no such hook, and its span
// keeps the method as its name.
func (p *Plugin) OnPageResolved(ctx context.Context, ev *collage.PageResolvedEvent) error {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() || ev.Page == nil {
		return nil
	}
	pattern, found := ev.Page.Paths[ev.Locale]
	if !found {
		return nil
	}
	// The locale prefix the router stripped is part of the route that matched.
	if prefix := "/" + ev.Locale; ev.Path == prefix || strings.HasPrefix(ev.Path, prefix+"/") {
		pattern = strings.TrimSuffix(prefix+pattern, "/")
		if pattern == "" {
			pattern = "/"
		}
	}
	// Only the span this plugin's middleware started is renamed; a span some
	// other middleware put in the context is not this plugin's to name.
	method, ok := ctx.Value(methodKey{}).(string)
	if !ok {
		return nil
	}
	span.SetName(method + " " + pattern)
	span.SetAttributes(
		attribute.String("http.route", pattern),
		attribute.String("collage.page", ev.Page.Name),
		attribute.String("collage.locale", ev.Locale),
	)
	return nil
}

// methodKey carries the request's method to OnPageResolved, which is handed the
// request's context but not the request.
type methodKey struct{}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// statusWriter records the status written through it. Flush and Hijack pass
// through, and Unwrap lets http.ResponseController reach the connection, so an
// event stream and a WebSocket work beneath it.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *statusWriter) Hijack() (conn net.Conn, rw *bufio.ReadWriter, err error) {
	if w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
