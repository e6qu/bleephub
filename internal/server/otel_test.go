package bleephub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestInitObservabilityNoEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	obs, err := InitObservability("test-service")
	if err != nil {
		t.Fatalf("InitObservability failed: %v", err)
	}
	defer obs.Shutdown(context.Background())

	tracer := otel.Tracer("test")
	_, span := tracer.Start(context.Background(), "test-span")
	defer span.End()

	if span.SpanContext().IsValid() {
		t.Error("expected no-op span when OTEL_EXPORTER_OTLP_ENDPOINT is unset")
	}
}

func TestInitObservabilityWithEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")

	obs, err := InitObservability("test-service")
	if err != nil {
		t.Fatalf("InitObservability failed: %v", err)
	}
	defer obs.Shutdown(context.Background())

	tracer := otel.Tracer("test")
	_, span := tracer.Start(context.Background(), "test-span")
	defer span.End()

	if !span.SpanContext().IsValid() {
		t.Error("expected valid SpanContext when OTEL_EXPORTER_OTLP_ENDPOINT is set")
	}

	otel.SetTracerProvider(noop.NewTracerProvider())
}

func TestHTTPMiddlewareCreatesSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(noop.NewTracerProvider())

	handler := otelhttp.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), "test-server")

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/test")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	resp.Body.Close()

	tp.ForceFlush(context.Background())

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected at least one span from HTTP middleware")
	}
}

func TestWorkflowDispatchCreatesSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(noop.NewTracerProvider())

	s := newTestServer()
	wf := &WorkflowDef{
		Name: "otel-test",
		Jobs: map[string]*JobDef{
			"build": {Steps: []StepDef{{Run: "echo hi"}}},
		},
	}

	_, err := s.submitWorkflow(context.Background(), "http://localhost", wf, "alpine:latest")
	if err != nil {
		t.Fatalf("submitWorkflow failed: %v", err)
	}

	tp.ForceFlush(context.Background())

	spans := exporter.GetSpans()
	found := false
	for _, span := range spans {
		if span.Name == "submitWorkflow" {
			found = true
			break
		}
	}
	if !found {
		names := make([]string, len(spans))
		for i, span := range spans {
			names[i] = span.Name
		}
		t.Errorf("expected submitWorkflow span, got: %v", names)
	}
}

func TestNoSpansWhenDisabled(t *testing.T) {
	// Use default no-op provider — should not crash
	otel.SetTracerProvider(noop.NewTracerProvider())

	s := newTestServer()
	wf := &WorkflowDef{
		Name: "no-trace-test",
		Jobs: map[string]*JobDef{
			"build": {Steps: []StepDef{{Run: "echo hi"}}},
		},
	}

	_, err := s.submitWorkflow(context.Background(), "http://localhost", wf, "alpine:latest")
	if err != nil {
		t.Fatalf("submitWorkflow with no-op tracer should not fail: %v", err)
	}
}

// TestOTelExporterConfiguredHonoursPerSignalEndpoints covers CORE-022: the
// enable-gate must recognise the per-signal OTLP endpoint variables, not only
// the general one, since the exporters honour them.
func TestOTelExporterConfiguredHonoursPerSignalEndpoints(t *testing.T) {
	for _, v := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	} {
		t.Run(v, func(t *testing.T) {
			// Clear the general var so only the per-signal one under test is set.
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv(v, "http://localhost:4318")
			if !otelExporterConfigured() {
				t.Fatalf("otelExporterConfigured() = false with %s set, want true", v)
			}
		})
	}
}

// TestInitObservabilityWithPerSignalEndpoint pins that only setting a per-signal
// endpoint still enables telemetry end-to-end (a valid SpanContext).
func TestInitObservabilityWithPerSignalEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://localhost:4318")

	obs, err := InitObservability("test-service")
	if err != nil {
		t.Fatalf("InitObservability failed: %v", err)
	}
	defer obs.Shutdown(context.Background())

	tracer := otel.Tracer("test")
	_, span := tracer.Start(context.Background(), "test-span")
	defer span.End()
	if !span.SpanContext().IsValid() {
		t.Error("expected a valid SpanContext when only OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is set")
	}

	otel.SetTracerProvider(noop.NewTracerProvider())
}
