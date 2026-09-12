package gb35114auth

import (
	"testing"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/config"
)

func TestBuildDisabledReturnsNil(t *testing.T) {
	auth, err := Build(config.GB35114Config{Enabled: false}, "34020000001320000001")
	if err != nil {
		t.Fatalf("Build disabled: %v", err)
	}
	if auth != nil {
		t.Fatalf("Build disabled returned %T, want nil", auth)
	}
}

func TestBuildValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.GB35114Config
		id   string
	}{
		{"missing device id", config.GB35114Config{Enabled: true, ServerID: "s", DeviceCertFile: "a", DeviceKeyFile: "b"}, ""},
		{"missing server id", config.GB35114Config{Enabled: true, DeviceCertFile: "a", DeviceKeyFile: "b"}, "d"},
		{"missing cert files", config.GB35114Config{Enabled: true, ServerID: "s"}, "d"},
	}
	for _, c := range cases {
		if _, err := Build(c.cfg, c.id); err == nil {
			t.Fatalf("%s: Build succeeded, want error", c.name)
		}
	}
}
