//go:build !gb35114

// Fallback for builds without the gb35114 tag: the A-level security
// capability is compile-time opt-in (it pulls the gmsm dependency), so a
// plain build warns and keeps SIP Digest.
package gb35114auth

import (
	"log/slog"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/config"
	gbdev "github.com/mickeyzzc/gb28181-go/device"
)

func newAuthenticator(cfg config.GB35114Config, deviceID string) (gbdev.RegisterAuthenticator, error) {
	slog.Warn("gb35114: enabled in config but this binary was built without -tags gb35114 — falling back to Digest auth")
	return nil, nil
}
