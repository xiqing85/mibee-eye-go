// Package otelx wires OpenTelemetry trace export (SPEC v1 §3.3 +
// appendix A #37): an OTLP gRPC exporter behind the
// `observability.otlp_endpoint` config key.
//
// An empty endpoint leaves tracing disabled (the global noop tracer);
// an unreachable collector fails open — one log line, the batch exporter
// retries on its own schedule, the camera pipeline is never affected.
package otelx

import (
	"context"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Init installs the W3C propagator and, when endpoint is non-empty, the
// OTLP-exporting tracer provider. Returns the shutdown function (safe to
// call even when disabled).
func Init(ctx context.Context, endpoint string) func(context.Context) error {
	// W3C TraceContext extraction/injection is harmless with the noop
	// tracer and lets http spans adopt inbound `traceparent` headers.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if endpoint == "" {
		return func(context.Context) error { return nil }
	}

	addr := stripScheme(endpoint)

	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(addr),
		// The collector is on the LAN; an insecure channel matches the
		// Rust twins' tonic default (no TLS on 4317).
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		slog.Warn("otel: exporter init failed — tracing stays off (fail-open)", "error", err, "endpoint", endpoint)
		return func(context.Context) error { return nil }
	}

	res, err := sdkresource.New(ctx,
		sdkresource.WithAttributes(semconv.ServiceName("mibee-eye")),
	)
	if err != nil {
		slog.Warn("otel: resource init failed — tracing stays off (fail-open)", "error", err)
		return func(context.Context) error { return nil }
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)
	slog.Info("otel: OTLP span export enabled", "endpoint", endpoint)

	return provider.Shutdown
}

// stripScheme accepts both "host:port" and "http(s)://host:port" forms —
// otlptracegrpc.WithEndpoint wants the bare address.
func stripScheme(endpoint string) string {
	for _, p := range []string{"http://", "https://"} {
		if strings.HasPrefix(endpoint, p) {
			return strings.TrimPrefix(endpoint, p)
		}
	}
	return endpoint
}
