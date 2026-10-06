package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/trace"
)

func TestContextLogsAndSafeErrors(t *testing.T) {
	const sensitive = "synthetic-token-phone-date-payload"
	for _, err := range []error{
		errors.New(sensitive),
		fmt.Errorf("%s: %w", sensitive, &pgconn.PgError{Code: "23514", Message: sensitive, Detail: sensitive}),
		&pgconn.PgError{Code: sensitive, Message: sensitive},
		fmt.Errorf("%s: %w", sensitive, context.DeadlineExceeded),
	} {
		var buffer bytes.Buffer
		logger := slog.New(newHandler(&buffer, slog.LevelDebug)).With("service", "registration-backend")
		sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled})
		ctx := trace.ContextWithSpanContext(context.Background(), sc)
		logger.LogAttrs(ctx, slog.LevelError, "persistence.failed", ErrorAttrs(err)...)
		if strings.Contains(buffer.String(), sensitive) {
			t.Fatal("sensitive error leaked")
		}
		var fields map[string]any
		if err := json.Unmarshal(buffer.Bytes(), &fields); err != nil {
			t.Fatal(err)
		}
		if fields["service"] != "registration-backend" || fields["trace_id"] != sc.TraceID().String() || fields["span_id"] != sc.SpanID().String() {
			t.Fatal("missing log context")
		}
		stamp, ok := fields["time"].(string)
		if !ok || !strings.HasSuffix(stamp, "Z") {
			t.Fatal("log time not UTC")
		}
		if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
			t.Fatal(err)
		}
		if fields["error_class"] == nil {
			t.Fatal("missing safe error classification")
		}
	}
}
