package bleephub

import (
	"context"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/embedded"
)

// captureOTelLogger records every emitted log record so the bridge's handling
// of malformed input can be asserted.
type captureOTelLogger struct {
	embedded.Logger
	records []otellog.Record
}

func (c *captureOTelLogger) Emit(_ context.Context, r otellog.Record) {
	c.records = append(c.records, r)
}

func (c *captureOTelLogger) Enabled(context.Context, otellog.EnabledParameters) bool { return true }

// TestOTelLogWriterForwardsMalformedLine is the CORE-008 regression: a log line
// that fails to parse as zerolog JSON must still reach OTel (emitted verbatim)
// rather than being reported as written and silently dropped.
func TestOTelLogWriterForwardsMalformedLine(t *testing.T) {
	cap := &captureOTelLogger{}
	w := &OTelLogWriter{logger: cap}

	valid := []byte(`{"level":"info","message":"hello","time":"2026-01-02T03:04:05Z"}`)
	if n, err := w.Write(valid); err != nil || n != len(valid) {
		t.Fatalf("Write(valid) = %d, %v; want %d, nil", n, err, len(valid))
	}

	bad := []byte("not json at all\n")
	if n, err := w.Write(bad); err != nil || n != len(bad) {
		t.Fatalf("Write(bad) = %d, %v; want %d, nil", n, err, len(bad))
	}

	if len(cap.records) != 2 {
		t.Fatalf("emitted %d records, want 2 (structured + malformed-fallback)", len(cap.records))
	}
	if got := cap.records[0].Body().AsString(); got != "hello" {
		t.Fatalf("structured record body = %q, want %q", got, "hello")
	}
	if got := cap.records[1].Body().AsString(); got != "not json at all" {
		t.Fatalf("malformed record body = %q, want the raw line", got)
	}
}
