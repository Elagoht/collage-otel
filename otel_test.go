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
	for _, page := range []*collage.Page{
		collage.NewPage("post").WithContent(collage.NewFragment("post", "p.html").Build()).WithPath("en", "/blog/{slug}").Build(),
		collage.NewPage("broken").WithContent(collage.NewFragment("broken", "p.html").Required().WithDataHandler(
			func(context.Context, *collage.RenderContext) (any, []string, error) { // any: DataHandlerFunc's own signature
				return nil, nil, errors.New("backend down")
			}).Build()).WithPath("en", "/broken").Build(),
	} {
		if err := app.RegisterPage(page); err != nil {
			t.Fatal(err)
		}
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
			return kv.Value.Emit(), true
		}
	}
	return "", false
}

// The caller's trace is continued: the request's span is a server span whose
// parent is the caller's, and collage's own spans nest under it.
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
		"collage.page":              "post",
		// What collage said about collage.http, before the span had started.
		"http.method":      "GET",
		"http.path":        "/blog/hello",
		"http.status_code": "200",
	} {
		if got, _ := attr(server, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for _, name := range []string{"collage.render", "collage.fragment"} {
		s, ok := spans[name]
		if !ok {
			t.Fatalf("no %s span; spans: %v", name, names(rec.Ended()))
		}
		if s.SpanContext().TraceID() != server.SpanContext().TraceID() {
			t.Errorf("%s is in another trace", name)
		}
	}
	if spans["collage.render"].Parent().SpanID() != server.SpanContext().SpanID() {
		t.Error("collage.render is not a child of the server span")
	}
	if len(rec.Ended()) != 3 {
		t.Errorf("spans = %v, want the server span, the render and the fragment", names(rec.Ended()))
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
	if server := spans["GET /broken"]; server == nil || server.Status().Code != codes.Error {
		t.Errorf("server span not an error: %v", names(rec.Ended()))
	}

	h, rec = site(t, setup{tracer: true, plugin: true})
	get(h, "/nowhere")
	server := byName(rec.Ended())["GET"]
	if server == nil || server.Status().Code == codes.Error {
		t.Errorf("a 404 is not the server's error: %v", names(rec.Ended()))
	}
	if got, _ := attr(server, "http.response.status_code"); got != "404" {
		t.Errorf("status = %q", got)
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

// As a plugin alone, the middleware's own span continues the caller's trace.
func TestPluginAlone(t *testing.T) {
	h, rec := site(t, setup{plugin: true})
	get(h, "/blog/hello", "traceparent", traceparent)
	ended := rec.Ended()
	if len(ended) != 1 || ended[0].Name() != "GET /blog/{slug}" || ended[0].SpanContext().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("spans %v", names(ended))
	}
}

// A skipped path is not traced by the middleware; collage's span for it is its
// own trace.
func TestSkip(t *testing.T) {
	h, rec := site(t, setup{tracer: true, plugin: true, config: `{"skip": ["/blog/"]}`})
	get(h, "/blog/hello", "traceparent", traceparent)
	root := byName(rec.Ended())["collage.http"]
	if root == nil || root.Parent().IsValid() {
		t.Errorf("spans %v", names(rec.Ended()))
	}
}

func TestBadConfigStopsStartup(t *testing.T) {
	h, _ := site(t, setup{plugin: true, config: `{"skip": ["blog"]}`})
	if code := get(h, "/blog/x").Code; code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", code)
	}
}

// A stream and a WebSocket beneath the middleware still reach the connection.
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
