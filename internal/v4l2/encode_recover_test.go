//go:build amd64 || arm64

package v4l2

import (
	"errors"
	"fmt"
	"testing"
)

// The Encode retry wrapper classifies errors by sentinel: usage errors
// (caller's fault) must not trigger a queue reset; transport errors must.
// These tests pin the classification without touching a device.

func TestEncodeUsageErrorsAreSentinelMarked(t *testing.T) {
	cases := []error{
		fmt.Errorf("%w: encoder closed", errEncodeUsage),
		fmt.Errorf("%w: short frame: got %d bytes, need %d", errEncodeUsage, 10, 20),
		fmt.Errorf("%w: output buffer too small for padded stride %d", errEncodeUsage, 64),
	}
	for _, err := range cases {
		if !errors.Is(err, errEncodeUsage) {
			t.Fatalf("usage error must carry the sentinel: %v", err)
		}
	}
}

func TestTransportErrorsAreNotSentinelMarked(t *testing.T) {
	cases := []error{
		fmt.Errorf("v4l2: encoder poll timeout (1000ms)"),
		fmt.Errorf("v4l2: encoder QBUF(type 10, idx 1): %w", errors.New("invalid argument")),
		fmt.Errorf("v4l2: encoder DQBUF(output): %w", errors.New("no such device")),
		fmt.Errorf("v4l2: encoder output has no Annex-B start code (0 bytes)"),
	}
	for _, err := range cases {
		if errors.Is(err, errEncodeUsage) {
			t.Fatalf("transport error must not be usage-classified: %v", err)
		}
	}
}
