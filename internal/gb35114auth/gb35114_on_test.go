//go:build gb35114

package gb35114auth

import (
	"path/filepath"
	"testing"

	sec "github.com/mickeyzzc/gb28181-go/security35114"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/config"
)

const (
	tDeviceID = "34020000001320000001"
	tServerID = "34020000002000000001"
)

func testCfg() config.GB35114Config {
	return config.GB35114Config{
		Enabled:          true,
		DeviceCertFile:   filepath.Join("testdata", "device_cert.pem"),
		DeviceKeyFile:    filepath.Join("testdata", "device_key.pem"),
		PlatformCertFile: filepath.Join("testdata", "platform_cert.pem"),
		ServerID:         tServerID,
	}
}

func TestBuildEnabledTagged(t *testing.T) {
	auth, err := Build(testCfg(), tDeviceID)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if auth == nil {
		t.Fatal("Build returned nil with gb35114 tag and enabled config")
	}
	if _, ok := auth.(interface{ VKEK() []byte }); !ok {
		t.Fatalf("Build returned %T, want the security35114 Authenticator", auth)
	}
	// The Capability announcement must carry the product's device ID.
	got := auth.InitialAuthorization()
	wantPrefix := `Capability algorithm="A:SM2;H:SM3;S:SM4/OFB/PKCS5;SI:SM3-SM2", keyversion="`
	if len(got) < len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("initial authorization = %q", got)
	}
}

func TestBuildEnabledBadFiles(t *testing.T) {
	cfg := testCfg()
	cfg.DeviceCertFile = filepath.Join("testdata", "missing.pem")
	if _, err := Build(cfg, tDeviceID); err == nil {
		t.Fatal("Build with missing cert file succeeded")
	}
}

// TestBuildHandshakeSmoke drives one full A-level handshake through the
// built authenticator: challenge → signed re-REGISTER → SecurityInfo with
// the VKEK sealed to the test device key. This pins the product wiring,
// not the library (the library has its own end-to-end suite).
func TestBuildHandshakeSmoke(t *testing.T) {
	auth, err := Build(testCfg(), tDeviceID)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	authz, err := auth.AuthorizeWithChallenge(
		`Unidirection algorithm="A:SM2;H:SM3;S:SM4/OFB/PKCS5;SI:SM3-SM2", random1="PRAIIbutDbd5x/NKsbwwYw=="`)
	if err != nil {
		t.Fatalf("AuthorizeWithChallenge: %v", err)
	}
	aa, err := sec.ParseAuthAuthorization(authz)
	if err != nil {
		t.Fatalf("ParseAuthAuthorization: %v", err)
	}
	// Platform side: verify the product-configured identity signed it.
	identity, err := sec.LoadIdentityFromFiles(
		filepath.Join("testdata", "device_cert.pem"), filepath.Join("testdata", "device_key.pem"))
	if err != nil {
		t.Fatalf("LoadIdentityFromFiles: %v", err)
	}
	payload := sec.SignAuthPayload(aa.Random1, aa.Random2, tServerID, sec.ConcatWireStrings)
	if err := sec.VerifyMessage(identity.Certificate, payload, aa.Sign1); err != nil {
		t.Fatalf("sign1 from product authenticator does not verify: %v", err)
	}
}
