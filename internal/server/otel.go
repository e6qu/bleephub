package bleephub

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Observability bundles trace + log SDK shutdown with a zerolog Writer that
// mirrors entries to the OTel logs SDK.
type Observability struct {
	LogWriter *OTelLogWriter
	Shutdown  func(context.Context) error
}

// otelExporterConfigured reports whether OTLP export is configured. The exporters
// honour per-signal endpoint variables too, so gating solely on the general
// OTEL_EXPORTER_OTLP_ENDPOINT silently disabled a per-signal-only setup (CORE-022).
func otelExporterConfigured() bool {
	for _, v := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	} {
		if strings.TrimSpace(os.Getenv(v)) != "" {
			return true
		}
	}
	return false
}

func InitObservability(serviceName string) (*Observability, error) {
	if !otelExporterConfigured() {
		return &Observability{
			Shutdown: func(context.Context) error { return nil },
		}, nil
	}
	res := resource.NewWithAttributes(
		semconv.SchemaURL, semconv.ServiceNameKey.String(serviceName),
	)

	traceExp, err := otlptracehttp.New(context.Background())
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	logExp, err := otlploghttp.New(context.Background())
	if err != nil {
		_ = tp.Shutdown(context.Background())
		return nil, err
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
		sdklog.WithResource(res),
	)
	global.SetLoggerProvider(lp)

	metricExp, err := otlpmetrichttp.New(context.Background())
	if err != nil {
		_ = tp.Shutdown(context.Background())
		_ = lp.Shutdown(context.Background())
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	// Tear down on a runtime-metrics failure rather than booting half-wired.
	if err := runtime.Start(runtime.WithMinimumReadMemStatsInterval(15 * time.Second)); err != nil {
		_ = tp.Shutdown(context.Background())
		_ = lp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
		return nil, err
	}

	return &Observability{
		LogWriter: &OTelLogWriter{logger: lp.Logger(serviceName)},
		Shutdown: func(ctx context.Context) error {
			return errors.Join(tp.Shutdown(ctx), lp.Shutdown(ctx), mp.Shutdown(ctx))
		},
	}, nil
}

// OTelLogWriter bridges zerolog to the OTel logs SDK as an io.Writer.
type OTelLogWriter struct {
	logger otellog.Logger
}

func (w *OTelLogWriter) Write(p []byte) (int, error) {
	if w == nil || w.logger == nil {
		return len(p), nil
	}
	var entry map[string]any
	if err := json.Unmarshal(p, &entry); err != nil {
		// Emit a non-JSON line verbatim rather than dropping it from the pipeline.
		var record otellog.Record
		record.SetObservedTimestamp(time.Now())
		record.SetBody(attribute.StringValue(strings.TrimRight(string(p), "\n")))
		record.SetSeverity(otellog.SeverityInfo)
		w.logger.Emit(context.Background(), record)
		return len(p), nil
	}
	var record otellog.Record
	record.SetTimestamp(parseZerologTimestamp(entry))
	record.SetObservedTimestamp(time.Now())
	if msg, ok := entry["message"].(string); ok {
		record.SetBody(attribute.StringValue(msg))
	}
	level, _ := entry["level"].(string)
	severity, severityText := zerologLevelToOTel(level)
	record.SetSeverity(severity)
	record.SetSeverityText(severityText)
	for k, v := range entry {
		switch k {
		// trace_id/span_id go into the record's trace context below, not attributes.
		case "level", "message", "time", "trace_id", "span_id":
			continue
		}
		record.AddAttributes(attribute.KeyValue{Key: attribute.Key(k), Value: otelValueOf(v)})
	}
	w.logger.Emit(logEmitContext(entry), record)
	return len(p), nil
}

// logEmitContext reconstructs a log line's trace context from its trace_id/span_id
// fields so the emitted record correlates with its span (CORE-007). A line without a
// valid pair emits under the background context.
func logEmitContext(entry map[string]any) context.Context {
	traceHex, _ := entry["trace_id"].(string)
	spanHex, _ := entry["span_id"].(string)
	if traceHex == "" || spanHex == "" {
		return context.Background()
	}
	traceID, err := trace.TraceIDFromHex(traceHex)
	if err != nil {
		return context.Background()
	}
	spanID, err := trace.SpanIDFromHex(spanHex)
	if err != nil {
		return context.Background()
	}
	return trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	}))
}

func parseZerologTimestamp(entry map[string]any) time.Time {
	if v, ok := entry["time"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
	}
	return time.Now()
}

func zerologLevelToOTel(level string) (otellog.Severity, string) {
	switch strings.ToLower(level) {
	case "trace":
		return otellog.SeverityTrace, "TRACE"
	case "debug":
		return otellog.SeverityDebug, "DEBUG"
	case "info":
		return otellog.SeverityInfo, "INFO"
	case "warn", "warning":
		return otellog.SeverityWarn, "WARN"
	case "error":
		return otellog.SeverityError, "ERROR"
	case "fatal":
		return otellog.SeverityFatal, "FATAL"
	case "panic":
		return otellog.SeverityFatal4, "PANIC"
	}
	return otellog.SeverityInfo, level
}

func otelValueOf(v any) attribute.Value {
	switch x := v.(type) {
	case nil:
		return attribute.StringValue("")
	case string:
		return attribute.StringValue(x)
	case bool:
		return attribute.BoolValue(x)
	case float64:
		if x == float64(int64(x)) {
			return attribute.Int64Value(int64(x))
		}
		return attribute.Float64Value(x)
	case int:
		return attribute.Int64Value(int64(x))
	case int64:
		return attribute.Int64Value(x)
	}
	if b, err := json.Marshal(v); err == nil {
		return attribute.StringValue(string(b))
	}
	return attribute.StringValue("")
}
