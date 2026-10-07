package otel_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"testing/fstest"

	otel "github.com/Elagoht/collage-otel"
	"github.com/Elagoht/collage/pkg/collage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// buildReader reads a finished build, as elagoht/deploy does; with one
// registered, the build asks the handler for every file to capture its headers.
type buildReader struct{ files []collage.BuiltFile }

func (*buildReader) Name() string                             { return "test/buildreader" }
func (*buildReader) Version() string                          { return "0" }
func (*buildReader) Init(context.Context, collage.Host) error { return nil }
func (*buildReader) Shutdown(context.Context) error           { return nil }
func (b *buildReader) OnBuildFinished(_ context.Context, ev *collage.BuildFinishedEvent) error {
	b.files = ev.Files
	return nil
}

// buildSpans builds a three-page site with the plugin as collage's Tracer and
// in Plugins, and returns the names of the spans the build ended. With reader
// set, a buildReader is registered, so the build captures every file's headers.
func buildSpans(t *testing.T, reader *buildReader) []string {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	p := otel.New(otel.Options{Tracer: tp.Tracer("test"), Propagator: propagation.TraceContext{}})
	plugins := []collage.Plugin{p}
	if reader != nil {
		plugins = append(plugins, reader)
	}
	app, err := collage.New(&collage.Config{
		Server: collage.ServerConfig{Host: "localhost", Port: 3000},
		Template: collage.TemplateConfig{FS: fstest.MapFS{
			"t/p.html": {Data: []byte(`<main>home</main>`)},
		}, Root: "t"},
		Observability: collage.ObservabilityConfig{Tracer: p},
		Plugins:       plugins,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/a", "/b"} {
		page := collage.NewPage("p"+path).WithContent(collage.NewFragment("home", "p.html").Build()).WithPath("en", path).Build()
		if err := app.RegisterPage(page); err != nil {
			t.Fatal(err)
		}
	}
	builder, err := collage.NewBuilder(app, collage.BuildOptions{OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	report, err := builder.Build(context.Background())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, f := range report.Findings {
		t.Errorf("finding: %s %s: %s", f.Rule, f.Path, f.Message)
	}
	var names []string
	for _, s := range rec.Ended() {
		names = append(names, s.Name())
	}
	slices.Sort(names)
	return names
}

// A static build's header capture is not traced: the build ends the same
// spans with its capture as without it — no server span, no collage.http, no
// render — and the captured headers carry no trace context.
func TestBuildCaptureIsNotTraced(t *testing.T) {
	without := buildSpans(t, nil)
	reader := &buildReader{}
	with := buildSpans(t, reader)
	if !slices.Equal(with, without) {
		t.Errorf("spans with the capture %v, without it %v", with, without)
	}
	captured := 0
	for _, f := range reader.files {
		if !f.Captured {
			continue
		}
		captured++
		if f.Status != http.StatusOK {
			t.Errorf("%s: status %d", f.Path, f.Status)
		}
		for _, h := range []string{"Traceparent", "Tracestate"} {
			if f.Headers.Get(h) != "" {
				t.Errorf("%s: the capture carries %s: %v", f.Path, h, f.Headers)
			}
		}
	}
	if captured != 3 {
		t.Fatalf("%d pages captured, want 3", captured)
	}
}
