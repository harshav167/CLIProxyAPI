package observability

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

var installLogrusHookOnce sync.Once

func InstallLogrusHook() {
	installLogrusHookOnce.Do(func() {
		logrus.AddHook(&LogrusHook{})
	})
}

// LogrusHook mirrors safe logrus entries into OpenTelemetry logs.
type LogrusHook struct{}

func (h *LogrusHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

func (h *LogrusHook) Fire(entry *logrus.Entry) error {
	if entry == nil || !Enabled() {
		return nil
	}
	record := otellog.Record{}
	timestamp := entry.Time
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	record.SetTimestamp(timestamp)
	record.SetObservedTimestamp(time.Now())
	record.SetBody(otellog.StringValue(RedactStringForLog(entry.Message, 2048)))
	record.SetSeverity(logrusSeverity(entry.Level))
	record.SetSeverityText(entry.Level.String())
	for key, value := range entry.Data {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		record.AddAttributes(otellog.String(key, safeLogValue(key, value)))
	}
	ctx := entry.Context
	if ctx == nil {
		ctx = context.Background()
	}
	spanContext := trace.SpanContextFromContext(ctx)
	if spanContext.IsValid() {
		record.AddAttributes(
			otellog.String("trace_id", spanContext.TraceID().String()),
			otellog.String("span_id", spanContext.SpanID().String()),
		)
	}
	Logger().Emit(ctx, record)
	return nil
}

func logrusSeverity(level logrus.Level) otellog.Severity {
	switch level {
	case logrus.PanicLevel, logrus.FatalLevel:
		return otellog.SeverityFatal
	case logrus.ErrorLevel:
		return otellog.SeverityError
	case logrus.WarnLevel:
		return otellog.SeverityWarn
	case logrus.InfoLevel:
		return otellog.SeverityInfo
	default:
		return otellog.SeverityDebug
	}
}

func safeLogValue(key string, value any) string {
	// Two layers of redaction:
	//   1. key-name match (isSecretName via safeHeaderValue) — drops the whole
	//      value for keys like "authorization", "api_key".
	//   2. value-pattern match (redactString) — catches secrets that ride on
	//      BENIGN keys like "error" or "body" (echoed upstream error bodies,
	//      stack traces with tokens). Without (2), a sk-/Bearer/AIza value on an
	//      "error" field shipped verbatim to SigNoz.
	redacted := safeHeaderValue(key, fmt.Sprint(value))
	if redacted == "<redacted>" {
		return redacted
	}
	return RedactStringForLog(redacted, 512)
}
