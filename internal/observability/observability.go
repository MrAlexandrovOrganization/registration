package observability

import (
	"context"
	"io"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type handler struct{ slog.Handler }

func (h handler) Handle(ctx context.Context, r slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}
func (h handler) WithAttrs(a []slog.Attr) slog.Handler { return handler{h.Handler.WithAttrs(a)} }
func (h handler) WithGroup(g string) slog.Handler      { return handler{h.Handler.WithGroup(g)} }
func newHandler(w io.Writer, level slog.Level) slog.Handler {
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.String("time", a.Value.Time().UTC().Format(time.RFC3339Nano))
		}
		return a
	}})
	return handler{base}
}

func Init(ctx context.Context, service string) (func(context.Context) error, error) {
	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(os.Getenv("LOG_LEVEL")))
	slog.SetDefault(slog.New(newHandler(os.Stdout, level)).With("service", service))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	// Export failures are optional infrastructure failures and never expose endpoint errors.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) { slog.Warn("telemetry export failed") }))
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", service))), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1)))}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		exporter, err := otlptracegrpc.New(ctx)
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}
	provider := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}
