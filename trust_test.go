package otel_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	otel "github.com/Elagoht/collage-otel"
	"github.com/Elagoht/collage/pkg/collage"
	"github.com/Elagoht/collage/pkg/collagetest"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const (
	callerTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerSpan  = "00f067aa0ba902b7"
	proxy       = "10.0.0.1"
	visitor     = "203.0.113.7"
)

// trustSite is a site behind the trusted proxy 10.0.0.1, with the plugin as
// collage's Tracer and as a plugin, the given sampler and configuration, a
// page at /blog/{slug} and a handler at /raw/ that reports the baggage it was
// served under.
func trustSite(t *testing.T, sampler sdktrace.Sampler, config string, baggageSeen *int) (*collagetest.Site, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec), sdktrace.WithSampler(sampler))
	p := otel.New(otel.Options{
		Tracer:     tp.Tracer("test"),
		Propagator: propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
	})
	opts := []collagetest.SiteOption{
		collagetest.WithoutHome(),
		collagetest.WithPlugins(p),
		collagetest.WithTemplates(map[string]string{"p.html": `<p>page</p>`}),
		collagetest.WithConfig(func(c *collage.Config) {
			c.Server.TrustedProxies = []string{proxy}
			c.Observability.Tracer = p
		}),
		collagetest.WithRegister(collage.NewPage("post").WithContent(collage.NewFragment("post", "p.html").Build()).WithPath("en", "/blog/{slug}").Build()),
		collagetest.WithSetup(func(app *collage.App) error {
			return app.Handle("/raw/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if baggageSeen != nil {
					*baggageSeen = baggage.FromContext(r.Context()).Len()
				}
				w.WriteHeader(http.StatusNoContent)
			}))
		}),
	}
	if config != "" {
		opts = append(opts, collagetest.WithPluginConfig(otel.Name, config))
	}
	return collagetest.NewSite(t, opts...), rec
}

// from sends GET path from remote with the caller's sampled traceparent.
func from(site *collagetest.Site, remote, path string, header ...string) {
	req := site.Request(http.MethodGet, path, nil)
	req.RemoteAddr = remote + ":4444"
	req.Header.Set("traceparent", "00-"+callerTrace+"-"+callerSpan+"-01")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	site.Do(req)
}

func serverSpan(t *testing.T, rec *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range rec.Ended() {
		if s.SpanKind() == trace.SpanKindServer {
			return s
		}
	}
	t.Fatalf("no server span; spans: %v", names(rec.Ended()))
	return nil
}

// PLUGA-C01, regression. An internet client with no trusted proxy in between
// cannot force sampling: under "sample none of my own", its sampled
// traceparent records nothing, and no span lands in the trace id it chose.
func TestAnUntrustedClientCannotForceSampling(t *testing.T) {
	site, rec := trustSite(t, sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0)), "", nil)
	for i := range 50 {
		from(site, visitor, fmt.Sprintf("/blog/p%d", i))
	}
	if n := len(rec.Ended()); n != 0 {
		t.Fatalf("%d spans recorded under ratio 0: the client's traceparent forced sampling", n)
	}
}

// An untrusted caller's trace is a link, not a parent: the server span is the
// root of a new trace, and its link names the caller's span.
func TestAnUntrustedTraceIsALink(t *testing.T) {
	site, rec := trustSite(t, sdktrace.AlwaysSample(), "", nil)
	from(site, visitor, "/blog/hello")
	server := serverSpan(t, rec)
	if server.Parent().IsValid() {
		t.Errorf("parent = %v, want none", server.Parent().SpanID())
	}
	if got := server.SpanContext().TraceID().String(); got == callerTrace {
		t.Error("the server span is in the client's trace")
	}
	links := server.Links()
	if len(links) != 1 || links[0].SpanContext.TraceID().String() != callerTrace || links[0].SpanContext.SpanID().String() != callerSpan {
		t.Errorf("links = %+v, want one to the caller's span", links)
	}
}

// A request through a trusted proxy continues the trace its headers carry.
func TestATrustedProxyContinuesTheTrace(t *testing.T) {
	site, rec := trustSite(t, sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0)), "", nil)
	from(site, proxy, "/blog/hello", "X-Forwarded-For", visitor)
	server := serverSpan(t, rec)
	if got := server.SpanContext().TraceID().String(); got != callerTrace {
		t.Errorf("trace id = %s, want the caller's", got)
	}
	if got := server.Parent().SpanID().String(); got != callerSpan {
		t.Errorf("parent = %s, want the caller's span", got)
	}
	if len(server.Links()) != 0 {
		t.Errorf("links = %v, want none", server.Links())
	}
}

// "always" continues anyone's trace; "never" continues no one's, a trusted
// proxy's either.
func TestTrustIncoming(t *testing.T) {
	for _, c := range []struct {
		config, remote string
		continued      bool
	}{
		{`{"trustIncoming": "always"}`, visitor, true},
		{`{"trustIncoming": "always"}`, proxy, true},
		{`{"trustIncoming": "never"}`, proxy, false},
		{`{"trustIncoming": "never"}`, visitor, false},
		{`{"trustIncoming": ""}`, proxy, true},
		{`{"trustIncoming": ""}`, visitor, false},
	} {
		site, rec := trustSite(t, sdktrace.AlwaysSample(), c.config, nil)
		from(site, c.remote, "/blog/hello")
		got := serverSpan(t, rec).SpanContext().TraceID().String() == callerTrace
		if got != c.continued {
			t.Errorf("%s from %s: continued = %v, want %v", c.config, c.remote, got, c.continued)
		}
	}
}

// The baggage an untrusted client sends does not reach the application.
func TestAnUntrustedClientsBaggageIsDropped(t *testing.T) {
	for remote, want := range map[string]int{visitor: 0, proxy: 1} {
		seen := -1
		site, _ := trustSite(t, sdktrace.AlwaysSample(), "", &seen)
		from(site, remote, "/raw/x", "baggage", "user=mallory")
		if seen != want {
			t.Errorf("from %s: %d baggage members reached the handler, want %d", remote, seen, want)
		}
	}
}

func TestTrustIncomingRefusesOtherValues(t *testing.T) {
	_, err := collagetest.TrySite(t,
		collagetest.WithPlugins(otel.New(otel.Options{})),
		collagetest.WithPluginConfig(otel.Name, `{"trustIncoming": "sometimes"}`))
	if err == nil || !strings.Contains(err.Error(), "otel: trustIncoming") {
		t.Fatalf("err = %v, want otel: trustIncoming ...", err)
	}
}

// PLUGA-C02, regression. A method outside RFC 9110's is "_OTHER" in the span
// name and in http.request.method, so invented methods cannot mint span names.
func TestAnInventedMethodIsOther(t *testing.T) {
	site, rec := trustSite(t, sdktrace.AlwaysSample(), "", nil)
	const n = 200
	for i := range n {
		site.Do(site.Request(fmt.Sprintf("X%05d", i), "/blog/hello", nil))
		site.Do(site.Request(fmt.Sprintf("Y%05d", i), "/no-such-page", nil))
	}
	site.Do(site.Request("get", "/blog/hello", nil))
	server := map[string]bool{}
	for _, s := range rec.Ended() {
		if s.SpanKind() != trace.SpanKindServer {
			continue
		}
		server[s.Name()] = true
		if got, _ := attr(s, "http.request.method"); got != "_OTHER" {
			t.Fatalf("http.request.method = %q, want _OTHER", got)
		}
		if got, _ := attr(s, "http.request.method_original"); !strings.HasPrefix(got, "X") && !strings.HasPrefix(got, "Y") && got != "get" {
			t.Fatalf("http.request.method_original = %q", got)
		}
	}
	// An invented method on a page's path may resolve to the page or to nothing,
	// as the router decides; either way the name is one of these two.
	allowed := map[string]bool{"HTTP /blog/{slug}": true, "HTTP": true}
	for name := range server {
		if !allowed[name] {
			t.Errorf("server span named %q; want one of %v", name, allowed)
		}
	}
	if len(server) == 0 {
		t.Fatal("no server spans")
	}
}

// Every RFC 9110 method, and PATCH, keeps its name.
func TestAKnownMethodKeepsItsName(t *testing.T) {
	site, rec := trustSite(t, sdktrace.AlwaysSample(), "", nil)
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "DELETE", "CONNECT", "OPTIONS", "TRACE", "PATCH"} {
		rec.Reset()
		site.Do(site.Request(m, "/no-such-page", nil))
		s := serverSpan(t, rec)
		if got, _ := attr(s, "http.request.method"); got != m || s.Name() != m {
			t.Errorf("%s: name %q, http.request.method %q", m, s.Name(), got)
		}
		if _, ok := attr(s, "http.request.method_original"); ok {
			t.Errorf("%s: method_original set for a known method", m)
		}
	}
}

// With no Server.TrustedProxies at all, the default trusts no one.
func TestWithNoTrustedProxiesNoOneIsTrusted(t *testing.T) {
	h, rec := site(t, setup{plugin: true})
	get(h, "/blog/hello", "traceparent", traceparent)
	ended := rec.Ended()
	if len(ended) != 1 || ended[0].SpanContext().TraceID().String() == callerTrace || len(ended[0].Links()) != 1 {
		t.Errorf("spans %v: want one new trace linking to the caller's", names(ended))
	}
}
