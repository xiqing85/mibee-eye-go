//go:build gb35114

// Real implementation: loads the provisioned SM2 identity and platform
// certificate and returns the security35114 authenticator.
package gb35114auth

import (
	"fmt"

	sec "github.com/mickeyzzc/gb28181-go/security35114"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/config"
	gbdev "github.com/mickeyzzc/gb28181-go/device"
)

func newAuthenticator(cfg config.GB35114Config, deviceID string) (gbdev.RegisterAuthenticator, error) {
	opts := sec.Options{
		DeviceID: deviceID,
		ServerID: cfg.ServerID,
	}
	identity, err := sec.LoadIdentityFromFiles(cfg.DeviceCertFile, cfg.DeviceKeyFile)
	if err != nil {
		return nil, fmt.Errorf("gb35114: loading device identity: %w", err)
	}
	opts.Device = identity
	if cfg.PlatformCertFile != "" {
		platformCert, err := sec.LoadCertificate(cfg.PlatformCertFile)
		if err != nil {
			return nil, fmt.Errorf("gb35114: loading platform certificate: %w", err)
		}
		opts.PlatformCert = platformCert
	}
	auth, err := sec.New(opts)
	if err != nil {
		return nil, fmt.Errorf("gb35114: %w", err)
	}
	return auth, nil
}
