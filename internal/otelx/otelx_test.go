package otelx

import (
	"context"
	"strings"
	"testing"
)

func TestStripScheme(t *testing.T) {
	cases := map[string]string{
		"http://collector:4317":  "collector:4317",
		"https://collector:4317": "collector:4317",
		"collector:4317":         "collector:4317",
		"192.168.1.10:4317":      "192.168.1.10:4317",
	}
	for in, want := range cases {
		if got := stripScheme(in); got != want {
			t.Errorf("stripScheme(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInitEmptyEndpointIsNoop(t *testing.T) {
	shutdown := Init(context.Background(), "")
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("noop shutdown must not error: %v", err)
	}
}

func TestInitUnreachableEndpointFailsOpen(t *testing.T) {
	// Building the gRPC client is lazy in otlptracegrpc — Init must
	// return without panicking; the connection failure surfaces later on
	// the batch exporter's own schedule (logged, not fatal).
	shutdown := Init(context.Background(), "http://127.0.0.1:19999")
	if err := shutdown(context.Background()); err != nil && !strings.Contains(err.Error(), "connection") {
		t.Logf("shutdown returned %v (acceptable — exporter draining)", err)
	}
}
