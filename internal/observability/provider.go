package observability

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const instrumentationName = "github.com/router-for-me/CLIProxyAPI/v7/internal/observability"

var (
	stateMu sync.RWMutex
	current *State
)

// State owns the OpenTelemetry SDK providers for a running service instance.
type State struct {
	settings       Settings
	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	loggerProvider *sdklog.LoggerProvider
}

// Start initializes OpenTelemetry providers. Disabled configuration returns a
// nil state and no error, so callers can keep startup behavior unchanged.
func Start(ctx context.Context, cfg *config.Config) (*State, error) {
	settings := Normalize(cfg)
	if !settings.Enabled {
		SetActive(nil)
		return nil, nil
	}
	if settings.Protocol != "" && settings.Protocol != defaultProtocol {
		return nil, fmt.Errorf("observability: unsupported OTLP protocol %q", settings.Protocol)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	res, err := newResource(ctx, settings)
	if err != nil {
		return nil, err
	}
	state := &State{settings: settings}

	if settings.Traces {
		traceExporter, errTrace := newTraceExporter(ctx, settings)
		if errTrace != nil {
			return nil, fmt.Errorf("observability: trace exporter: %w", errTrace)
		}
		state.tracerProvider = sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(traceExporter),
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.TraceIDRatioBased(settings.SampleRatio)),
		)
		otel.SetTracerProvider(state.tracerProvider)
	}

	if settings.Metrics {
		metricExporter, errMetric := newMetricExporter(ctx, settings)
		if errMetric != nil {
			return nil, fmt.Errorf("observability: metric exporter: %w", errMetric)
		}
		reader := sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(15*time.Second))
		state.meterProvider = sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(reader),
			sdkmetric.WithResource(res),
			sdkmetric.WithCardinalityLimit(2000),
		)
		otel.SetMeterProvider(state.meterProvider)
		setProxyMetrics(newProxyMetrics())
		// Rebind runtime instrumentation to THIS meter provider on every
		// Start. A process-global sync.Once would bind go.memory.* /
		// go.goroutine.count / go.config.gogc to the first provider, which is
		// then shut down on the next config reload — flatlining all Go-runtime
		// metrics after the first reload. runtime.Start registers async
		// callbacks scoped to the provided meter provider, so calling it once
		// per fresh provider is correct: the old provider's callbacks die with
		// it on Shutdown, the new ones report against the live provider.
		if errRuntime := runtime.Start(runtime.WithMeterProvider(state.meterProvider)); errRuntime != nil {
			logrus.WithError(errRuntime).Warn("observability: runtime metrics disabled")
		}
	}

	if settings.Logs {
		logExporter, errLog := newLogExporter(ctx, settings)
		if errLog != nil {
			return nil, fmt.Errorf("observability: log exporter: %w", errLog)
		}
		state.loggerProvider = sdklog.NewLoggerProvider(
			sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
			sdklog.WithResource(res),
		)
		global.SetLoggerProvider(state.loggerProvider)
		InstallLogrusHook()
	}

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	SetActive(state)
	return state, nil
}

// Shutdown flushes and stops all configured providers.
func (s *State) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var errs []error
	if s.tracerProvider != nil {
		if err := s.tracerProvider.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if s.meterProvider != nil {
		if err := s.meterProvider.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if s.loggerProvider != nil {
		if err := s.loggerProvider.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	SetActive(nil)
	return errors.Join(errs...)
}

// NewSettingsOnlyStateForTest builds an active State that carries only
// normalized Settings (no real OTel providers/exporters). It exists so tests
// in other packages can exercise the cheap live-state accessors
// (TransportLogsActive / TransportLogsFullBodyActive) without standing up an
// OTLP exporter. Production code must use Start.
func NewSettingsOnlyStateForTest(transportLogs, transportLogsFullBody bool) *State {
	return &State{settings: Settings{
		Enabled:               true,
		TransportLogs:         transportLogs,
		TransportLogsFullBody: transportLogs && transportLogsFullBody,
	}}
}

// SetActive publishes the active state for instrumentation helpers.
func SetActive(state *State) {
	stateMu.Lock()
	current = state
	stateMu.Unlock()
}

func active() *State {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return current
}

func Tracer() trace.Tracer {
	return otel.Tracer(instrumentationName)
}

func Logger() otellog.Logger {
	return global.Logger(instrumentationName)
}

func Enabled() bool {
	return active() != nil
}

// TransportLogsActive reports whether transport-summary logging is enabled on
// the live observability state. It reads the already-normalized settings on the
// active provider — no env lookups, no map allocations — so it is cheap enough
// to call on the per-SSE-chunk streaming hot path as a gate BEFORE doing any
// Normalize() work. Returns false when observability is disabled.
func TransportLogsActive() bool {
	state := active()
	return state != nil && state.settings.TransportLogs
}

// TransportLogsFullBodyActive reports whether full request/response body
// capture is enabled on the live state. Same cheap-read contract as
// TransportLogsActive.
func TransportLogsFullBodyActive() bool {
	state := active()
	return state != nil && state.settings.TransportLogs && state.settings.TransportLogsFullBody
}

func newTraceExporter(ctx context.Context, settings Settings) (*otlptrace.Exporter, error) {
	options := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(traceEndpoint(settings.Endpoint)),
		otlptracehttp.WithHeaders(settings.Headers),
	}
	if shouldUseInsecure(settings) {
		options = append(options, otlptracehttp.WithInsecure())
	} else {
		options = append(options, otlptracehttp.WithTLSClientConfig(&tls.Config{MinVersion: tls.VersionTLS12}))
	}
	return otlptracehttp.New(ctx, options...)
}

func newMetricExporter(ctx context.Context, settings Settings) (*otlpmetrichttp.Exporter, error) {
	options := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpointURL(metricEndpoint(settings.Endpoint)),
		otlpmetrichttp.WithHeaders(settings.Headers),
	}
	if shouldUseInsecure(settings) {
		options = append(options, otlpmetrichttp.WithInsecure())
	} else {
		options = append(options, otlpmetrichttp.WithTLSClientConfig(&tls.Config{MinVersion: tls.VersionTLS12}))
	}
	return otlpmetrichttp.New(ctx, options...)
}

func newLogExporter(ctx context.Context, settings Settings) (*otlploghttp.Exporter, error) {
	options := []otlploghttp.Option{
		otlploghttp.WithEndpointURL(logEndpoint(settings.Endpoint)),
		otlploghttp.WithHeaders(settings.Headers),
	}
	if shouldUseInsecure(settings) {
		options = append(options, otlploghttp.WithInsecure())
	} else {
		options = append(options, otlploghttp.WithTLSClientConfig(&tls.Config{MinVersion: tls.VersionTLS12}))
	}
	return otlploghttp.New(ctx, options...)
}

func shouldUseInsecure(settings Settings) bool {
	if settings.Insecure {
		return true
	}
	parsed, err := url.Parse(settings.Endpoint)
	return err == nil && parsed.Scheme == "http"
}

func newResource(ctx context.Context, settings Settings) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		attribute.String("service.name", settings.ServiceName),
		attribute.String("deployment.environment", settings.Environment),
		attribute.String("service.namespace", "cliproxy"),
		attribute.String("service.instance.id", instanceID()),
		attribute.String("service.repository.url", "https://github.com/router-for-me/CLIProxyAPI"),
	}
	for key, value := range settings.ResourceAttributes {
		if key == "" || value == "" {
			continue
		}
		attrs = append(attrs, attribute.String(key, value))
	}
	return resource.New(ctx, resource.WithTelemetrySDK(), resource.WithAttributes(attrs...))
}

func instanceID() string {
	host, _ := os.Hostname()
	if host != "" {
		return host
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(buf)
}
