// Package gb35114auth bridges the product's GB35114 configuration to the
// gb28181-go security35114 authenticator (GB 35114 A-level, build-tagged).
// The Build entry point is defined once per build variant: the gb35114
// build constructs the real SM2 authenticator; builds without the tag
// warn and fall back to SIP Digest.
package gb35114auth

import (
	"fmt"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/config"
	gbdev "github.com/mickeyzzc/gb28181-go/device"
)

// Build constructs the REGISTER authenticator from the product config.
// A disabled or absent gb35114 section returns (nil, nil) — the device
// server keeps its built-in SIP Digest flow. deviceID is the GB28181
// device ID from the same section.
func Build(cfg config.GB35114Config, deviceID string) (gbdev.RegisterAuthenticator, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if err := validate(cfg, deviceID); err != nil {
		return nil, err
	}
	return newAuthenticator(cfg, deviceID)
}

func validate(cfg config.GB35114Config, deviceID string) error {
	if deviceID == "" {
		return fmt.Errorf("gb35114: gb28181.device_id is required")
	}
	if cfg.ServerID == "" {
		return fmt.Errorf("gb35114: gb35114.server_id is required")
	}
	if cfg.DeviceCertFile == "" || cfg.DeviceKeyFile == "" {
		return fmt.Errorf("gb35114: device_cert_file and device_key_file are required")
	}
	return nil
}
