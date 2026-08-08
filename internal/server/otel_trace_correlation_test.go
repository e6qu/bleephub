package bleephub

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"
)

// captureLogExporter records every emitted log record so a test can inspect the
// trace context the OTel logs SDK stamped on it.
type captureLogExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (e *captureLogExporter) Export(_ context.Context, recs []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range recs {
		e.records = append(e.records, recs[i].Clone())
	}
	return nil
}
func (e *captureLogExporter) Shutdown(context.Context) error   { return nil }
func (e *captureLogExporter) ForceFlush(context.Context) error { return nil }

// TestOTelLogBridgeCorrelatesTrace pins CORE-007: a request log line carrying
// trace_id/span_id is emitted under a reconstructed trace context, so the OTel
// logs SDK stamps the record's trace_id/span_id and logs correlate with their
// span — rather than being emitted under context.Background() as before.
func TestOTelLogBridgeCorrelatesTrace(t *testing.T) {
	exp := &captureLogExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })
	w := &OTelLogWriter{logger: lp.Logger("test")}

	const traceHex = "0123456789abcdef0123456789abcdef"
	const spanHex = "0123456789abcdef"
	line := `{"level":"debug","message":"request","trace_id":"` + traceHex + `","span_id":"` + spanHex + `","path":"/api/v3/repos/x/y"}` + "\n"
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if len(exp.records) != 1 {
		t.Fatalf("got %d records, want 1", len(exp.records))
	}
	rec := exp.records[0]
	if got := rec.TraceID().String(); got != traceHex {
		t.Fatalf("record trace_id = %q, want %q (log not correlated with its trace)", got, traceHex)
	}
	if got := rec.SpanID().String(); got != spanHex {
		t.Fatalf("record span_id = %q, want %q", got, spanHex)
	}
	// The promoted trace fields must not also appear as free-form attributes.
	rec.WalkAttributes(func(kv attribute.KeyValue) bool {
		if k := string(kv.Key); k == "trace_id" || k == "span_id" {
			t.Fatalf("trace field %q was duplicated as a log attribute", k)
		}
		return true
	})
}

// TestOTelLogBridgeSpanlessLineEmits pins that a line without trace fields still
// emits (under the background context) with a zero trace id — spanless
// startup/background logs keep flowing, they just do not correlate.
func TestOTelLogBridgeSpanlessLineEmits(t *testing.T) {
	exp := &captureLogExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })
	w := &OTelLogWriter{logger: lp.Logger("test")}

	if _, err := w.Write([]byte(`{"level":"info","message":"bleephub listening"}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(exp.records) != 1 {
		t.Fatalf("got %d records, want 1", len(exp.records))
	}
	if exp.records[0].TraceID().IsValid() {
		t.Fatalf("spanless line unexpectedly carried a valid trace id")
	}
}

// logEmitContext is exercised indirectly above; this guards its hex-parse
// rejection path so a malformed trace field cannot poison the emit context.
func TestLogEmitContextRejectsBadHex(t *testing.T) {
	ctx := logEmitContext(map[string]any{"trace_id": "not-hex", "span_id": "0123456789abcdef"})
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		t.Fatalf("malformed trace_id produced a trace context: %s", sc.TraceID())
	}
}
