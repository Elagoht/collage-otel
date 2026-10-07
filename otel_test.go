package otel_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	otel "github.com/Elagoht/collage-otel"
	"github.com/Elagoht/collage/pkg/collage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

type setup struct {
	tracer  bool // as collage's Tracer
	plugin  bool // in Plugins
	config  string
	handler http.Handler // mounted at /raw/
	locales bool         // en and tr, prefixed
}

func site(t *testing.T, s setup) (http.Handler, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	p := otel.New(otel.Options{Tracer: tp.Tracer("test"), Propagator: propagation.TraceContext{}})
	cfg := &collage.Config{
		Server: collage.ServerConfig{Host: "localhost", Port: 3000},
		Template: collage.TemplateConfig{FS: fstest.MapFS{
			"t/p.html": {Data: []byte(`<p>page</p>`)},
		}, Root: "t"},
	}
	if s.locales {
		cfg.Locale = collage.LocaleConfig{Default: "en", Supported: []string{"en", "tr"}}
	}
	if s.tracer {
		cfg.Observability.Tracer = p
	}
	if s.plugin {
		cfg.Plugins = []collage.Plugin{p}
	}
	if s.config != "" {
		cfg.PluginConfig = map[string]json.RawMessage{otel.Name: json.RawMessage(s.config)}
	}
	app, err := collage.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	post := collage.NewPage("post").WithContent(collage.NewFragment("post", "p.html").Build()).WithPath("en", "/blog/{slug}")
	// Only where "tr" is supported: collage refuses a path no URL reaches.
	if s.locales {
		post.WithPath("tr", "/blog/{slug}")
	}
	for _, page := range []*collage.Page{
		post.Build(),
		collage.NewPage("broken").WithContent(collage.NewFragment("broken", "p.html").Required().WithData(collage.Load(
			func(context.Context, *collage.RenderContext) (string, error) {
				return "", errors.New("backend down")
			})).Build()).WithPath("en", "/broken").Build(),
	} {
		if err := app.RegisterPage(page); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.RegisterDocument(collage.NewDocument("feed", "application/xml").AtRoot("/feed.xml").WithBody([]byte("<feed/>")).Build()); err != nil {
		t.Fatal(err)
	}
	if s.handler != nil {
		if err := app.Handle("/raw/", s.handler); err != nil {
			t.Fatal(err)
		}
	}
	return app.Handler(), rec
}

func get(h http.Handler, path string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func byName(spans []sdktrace.ReadOnlySpan) map[string]sdktrace.ReadOnlySpan {
	out := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		out[s.Name()] = s
	}
	return out
}

func attr(s sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.String(), true
		}
	}
	return "", false
}

// The caller's trace is continued: the server span is the root of the request,
// a child of the caller's remote span, and collage's own spans nest under it —
// collage.http first, then the render and the fragment.
func TestContinuesTheIncomingTrace(t *testing.T) {
	h, rec := site(t, setup{tracer: true, plugin: true})
	if code := get(h, "/blog/hello", "traceparent", traceparent).Code; code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	spans := byName(rec.Ended())
	server, ok := spans["GET /blog/{slug}"]
	if !ok {
		t.Fatalf("no server span named for the route; spans: %v", names(rec.Ended()))
	}
	if server.SpanKind() != trace.SpanKindServer {
		t.Errorf("kind = %v", server.SpanKind())
	}
	if got := server.SpanContext().TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace id = %s: the caller's trace was not continued", got)
	}
	if got := server.Parent().SpanID().String(); got != "00f067aa0ba902b7" || !server.Parent().IsRemote() {
		t.Errorf("parent = %s (remote %v)", got, server.Parent().IsRemote())
	}
	for key, want := range map[string]string{
		"http.request.method":       "GET",
		"http.route":                "/blog/{slug}",
		"http.response.status_code": "200",
		"url.path":                  "/blog/hello",
		"collage.page":              "post",
		"collage.route.kind":        "page",
		"collage.route.name":        "post",
	} {
		if got, _ := attr(server, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for child, parent := range map[string]sdktrace.ReadOnlySpan{
		"collage.http":     server,
		"collage.render":   spans["collage.http"],
		"collage.fragment": spans["collage.render"],
	} {
		s, ok := spans[child]
		if !ok || parent == nil {
			t.Fatalf("no %s span; spans: %v", child, names(rec.Ended()))
		}
		if s.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("%s is not a child of %s", child, parent.Name())
		}
		if s.SpanContext().TraceID() != server.SpanContext().TraceID() {
			t.Errorf("%s is in another trace", child)
		}
	}
	// collage's own attributes stay on its own span.
	if got, _ := attr(spans["collage.http"], "http.status_code"); got != "200" {
		t.Errorf("collage.http status = %q", got)
	}
	if len(rec.Ended()) != 4 {
		t.Errorf("spans = %v, want the server span, collage.http, the render and the fragment", names(rec.Ended()))
	}
}

// A document and a handler are named for their route too: a document by the
// pattern it was registered with, like a page, a handler by its prefix.
func TestNonPageRoutes(t *testing.T) {
	h, rec := site(t, setup{tracer: true, plugin: true, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})})
	for path, want := range map[string][3]string{
		"/feed.xml": {"GET /feed.xml", "document", "/feed.xml"},
		"/raw/x":    {"GET /raw/", "handler", "/raw/"},
	} {
		rec.Reset()
		get(h, path)
		server := byName(rec.Ended())[want[0]]
		if server == nil {
			t.Errorf("%s: no span %q; spans: %v", path, want[0], names(rec.Ended()))
			continue
		}
		if got, _ := attr(server, "collage.route.kind"); got != want[1] {
			t.Errorf("%s: kind = %q", path, got)
		}
		if got, _ := attr(server, "http.route"); got != want[2] {
			t.Errorf("%s: route = %q", path, got)
		}
	}
}

// The locale prefix the router stripped is part of the route.
func TestLocalePrefix(t *testing.T) {
	h, rec := site(t, setup{tracer: true, plugin: true, locales: true})
	if code := get(h, "/tr/blog/merhaba").Code; code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	server := byName(rec.Ended())["GET /tr/blog/{slug}"]
	if server == nil {
		t.Fatalf("spans %v", names(rec.Ended()))
	}
	if got, _ := attr(server, "collage.locale"); got != "tr" {
		t.Errorf("locale = %q", got)
	}
}

func names(spans []sdktrace.ReadOnlySpan) []string {
	var out []string
	for _, s := range spans {
		out = append(out, s.Name())
	}
	return out
}

func TestWithoutIncomingContextStartsATrace(t *testing.T) {
	h, rec := site(t, setup{tracer: true, plugin: true})
	get(h, "/blog/hello")
	server := byName(rec.Ended())["GET /blog/{slug}"]
	if server == nil || server.Parent().IsValid() {
		t.Fatalf("spans %v", names(rec.Ended()))
	}
}

// A failed fragment is an error on its span, and a 5xx on the server span; a 404
// is the client's and is not.
func TestErrors(t *testing.T) {
	h, rec := site(t, setup{tracer: true, plugin: true})
	if code := get(h, "/broken").Code; code != http.StatusInternalServerError {
		t.Fatalf("status %d", code)
	}
	spans := byName(rec.Ended())
	fragment := spans["collage.fragment"]
	if fragment == nil || fragment.Status().Code != codes.Error || len(fragment.Events()) == 0 || fragment.Events()[0].Name != "exception" {
		t.Errorf("fragment span does not record the error: %+v", fragment)
	}
	server := spans["GET /broken"]
	if server == nil || server.Status().Code != codes.Error {
		t.Fatalf("server span not an error: %v", names(rec.Ended()))
	}
	if got, _ := attr(server, "http.response.status_code"); got != "500" {
		t.Errorf("status = %q", got)
	}
	if got, _ := attr(server, "http.route"); got != "/broken" {
		t.Errorf("route = %q", got)
	}

	h, rec = site(t, setup{tracer: true, plugin: true})
	get(h, "/nowhere")
	server = byName(rec.Ended())["GET"]
	if server == nil || server.Status().Code == codes.Error {
		t.Errorf("a 404 is not the server's error: %v", names(rec.Ended()))
	}
	if got, _ := attr(server, "http.response.status_code"); got != "404" {
		t.Errorf("status = %q", got)
	}
	if _, ok := attr(server, "http.route"); ok {
		t.Error("an unresolved request has no route")
	}
}

// As collage's Tracer alone, the request's span is started when collage asks,
// and collage's spans still nest under it.
func TestTracerAlone(t *testing.T) {
	h, rec := site(t, setup{tracer: true})
	get(h, "/blog/hello", "traceparent", traceparent)
	spans := byName(rec.Ended())
	root := spans["collage.http"]
	if root == nil || root.Parent().IsValid() {
		t.Fatalf("spans %v", names(rec.Ended()))
	}
	if spans["collage.render"].Parent().SpanID() != root.SpanContext().SpanID() {
		t.Error("collage.render is not a child of collage.http")
	}
}

// As a plugin alone, the server span continues the caller's trace, and is the
// only span.
func TestPluginAlone(t *testing.T) {
	h, rec := site(t, setup{plugin: true})
	get(h, "/blog/hello", "traceparent", traceparent)
	ended := rec.Ended()
	if len(ended) != 1 || ended[0].Name() != "GET /blog/{slug}" || ended[0].SpanContext().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("spans %v", names(ended))
	}
}

// A skipped path gets no server span; collage's span for it is its own trace.
func TestSkip(t *testing.T) {
	h, rec := site(t, setup{tracer: true, plugin: true, config: `{"skip": ["/blog/"]}`})
	get(h, "/blog/hello", "traceparent", traceparent)
	root := byName(rec.Ended())["collage.http"]
	if root == nil || root.Parent().IsValid() || len(rec.Ended()) != 3 {
		t.Errorf("spans %v", names(rec.Ended()))
	}
}

func TestBadConfigStopsStartup(t *testing.T) {
	h, _ := site(t, setup{plugin: true, config: `{"skip": ["blog"]}`})
	if code := get(h, "/blog/x").Code; code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", code)
	}
}

// Nothing wraps the response writer: a stream and a WebSocket reach the
// connection.
func TestFlushAndHijackPassThrough(t *testing.T) {
	var flushed, hijackable bool
	h, _ := site(t, setup{tracer: true, plugin: true, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, isFlusher := w.(http.Flusher)
		flushed = isFlusher && http.NewResponseController(w).Flush() == nil
		_, hijackable = w.(http.Hijacker)
	})})
	if code := get(h, "/raw/stream").Code; code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	if !flushed || !hijackable {
		t.Errorf("flush %v, hijacker %v", flushed, hijackable)
	}
}
