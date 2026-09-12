package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTempYAML creates a temporary YAML file with the given content
// and returns its path. The file is cleaned up at the end of the test.
func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	// Minimal YAML — only required top-level keys to test defaults fill in
	cfgYAML := `
camera: {}
rtsp: {}
onvif: {}
device: {}
logging: {}
`
	path := writeTempYAML(t, cfgYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Camera defaults
	if cfg.Camera.Device != "/dev/video0" {
		t.Errorf("Camera.Device = %q, want %q", cfg.Camera.Device, "/dev/video0")
	}
	if cfg.Camera.Width != 1280 {
		t.Errorf("Camera.Width = %d, want %d", cfg.Camera.Width, 1280)
	}
	if cfg.Camera.Height != 720 {
		t.Errorf("Camera.Height = %d, want %d", cfg.Camera.Height, 720)
	}
	if cfg.Camera.FPS != 15 {
		t.Errorf("Camera.FPS = %d, want %d", cfg.Camera.FPS, 15)
	}
	if cfg.Camera.Codec != "h264" {
		t.Errorf("Camera.Codec = %q, want %q", cfg.Camera.Codec, "h264")
	}
	if cfg.Camera.Bitrate != 2_000_000 {
		t.Errorf("Camera.Bitrate = %d, want %d", cfg.Camera.Bitrate, 2_000_000)
	}
	if cfg.Camera.Brightness != 0.0 {
		t.Errorf("Camera.Brightness = %f, want %f", cfg.Camera.Brightness, 0.0)
	}
	if cfg.Camera.Contrast != 1.0 {
		t.Errorf("Camera.Contrast = %f, want %f", cfg.Camera.Contrast, 1.0)
	}
	if cfg.Camera.Saturation != 1.0 {
		t.Errorf("Camera.Saturation = %f, want %f", cfg.Camera.Saturation, 1.0)
	}
	if cfg.Camera.Sharpness != 1.0 {
		t.Errorf("Camera.Sharpness = %f, want %f", cfg.Camera.Sharpness, 1.0)
	}
	if cfg.Camera.HFlip {
		t.Error("Camera.HFlip default = true, want false")
	}
	if cfg.Camera.VFlip {
		t.Error("Camera.VFlip default = true, want false")
	}

	// RTSP defaults
	if cfg.RTSP.Port != 8554 {
		t.Errorf("RTSP.Port = %d, want %d", cfg.RTSP.Port, 8554)
	}
	if cfg.RTSP.Username != "" {
		t.Errorf("RTSP.Username = %q, want empty", cfg.RTSP.Username)
	}
	if cfg.RTSP.Password != "" {
		t.Errorf("RTSP.Password = %q, want empty", cfg.RTSP.Password)
	}

	// ONVIF defaults
	if cfg.ONVIF.Port != 8080 {
		t.Errorf("ONVIF.Port = %d, want %d", cfg.ONVIF.Port, 8080)
	}
	if cfg.ONVIF.Username != "admin" {
		t.Errorf("ONVIF.Username = %q, want %q", cfg.ONVIF.Username, "admin")
	}
	if cfg.ONVIF.Password != "" {
		t.Errorf("ONVIF.Password = %q, want empty", cfg.ONVIF.Password)
	}

	// Device defaults
	if cfg.Device.Name != "Pi Camera V1" {
		t.Errorf("Device.Name = %q, want %q", cfg.Device.Name, "Pi Camera V1")
	}
	if cfg.Device.Manufacturer != "Raspberry Pi" {
		t.Errorf("Device.Manufacturer = %q, want %q", cfg.Device.Manufacturer, "Raspberry Pi")
	}
	if cfg.Device.Model != "OV5647" {
		t.Errorf("Device.Model = %q, want %q", cfg.Device.Model, "OV5647")
	}
	if cfg.Device.Firmware != "1.0.0" {
		t.Errorf("Device.Firmware = %q, want %q", cfg.Device.Firmware, "1.0.0")
	}
	if cfg.Device.HardwareID != "OV5647" {
		t.Errorf("Device.HardwareID = %q, want %q", cfg.Device.HardwareID, "OV5647")
	}
	if cfg.Device.SerialNumber != "" {
		t.Errorf("Device.SerialNumber = %q, want empty", cfg.Device.SerialNumber)
	}

	// Logging defaults
	if cfg.Logging.Level != "info" {
		t.Errorf("Logging.Level = %q, want %q", cfg.Logging.Level, "info")
	}
}

func TestLoadFromFile(t *testing.T) {
	cfgYAML := `
camera:
  device: /dev/video1
  width: 640
  height: 480
  fps: 30
  codec: h265
  bitrate: 1000000
  brightness: -0.5
  contrast: 2.0
  saturation: 1.5
  sharpness: 3.0
  hflip: true
  vflip: true
rtsp:
  port: 8555
  username: "testuser"
  password: "testpass"
onvif:
  port: 8081
  username: "onvifuser"
  password: "onvifpass"
device:
  name: "Test Camera"
  manufacturer: "TestCorp"
  model: "TC-1000"
  firmware: "2.0.0"
  hardware_id: "TC1000"
  serial_number: "SN-001"
logging:
  level: "debug"
`
	path := writeTempYAML(t, cfgYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Camera
	if cfg.Camera.Device != "/dev/video1" {
		t.Errorf("Camera.Device = %q", cfg.Camera.Device)
	}
	if cfg.Camera.Width != 640 {
		t.Errorf("Camera.Width = %d", cfg.Camera.Width)
	}
	if cfg.Camera.Height != 480 {
		t.Errorf("Camera.Height = %d", cfg.Camera.Height)
	}
	if cfg.Camera.FPS != 30 {
		t.Errorf("Camera.FPS = %d", cfg.Camera.FPS)
	}
	if cfg.Camera.Codec != "h265" {
		t.Errorf("Camera.Codec = %q", cfg.Camera.Codec)
	}
	if cfg.Camera.Bitrate != 1_000_000 {
		t.Errorf("Camera.Bitrate = %d", cfg.Camera.Bitrate)
	}
	if !cfg.Camera.HFlip {
		t.Error("Camera.HFlip = false, want true")
	}
	if !cfg.Camera.VFlip {
		t.Error("Camera.VFlip = false, want true")
	}
	if cfg.Camera.Brightness != -0.5 {
		t.Errorf("Camera.Brightness = %f", cfg.Camera.Brightness)
	}
	if cfg.Camera.Contrast != 2.0 {
		t.Errorf("Camera.Contrast = %f", cfg.Camera.Contrast)
	}
	if cfg.Camera.Saturation != 1.5 {
		t.Errorf("Camera.Saturation = %f", cfg.Camera.Saturation)
	}
	if cfg.Camera.Sharpness != 3.0 {
		t.Errorf("Camera.Sharpness = %f", cfg.Camera.Sharpness)
	}

	// RTSP
	if cfg.RTSP.Port != 8555 {
		t.Errorf("RTSP.Port = %d", cfg.RTSP.Port)
	}
	if cfg.RTSP.Username != "testuser" {
		t.Errorf("RTSP.Username = %q", cfg.RTSP.Username)
	}
	if cfg.RTSP.Password != "testpass" {
		t.Errorf("RTSP.Password = %q", cfg.RTSP.Password)
	}

	// ONVIF
	if cfg.ONVIF.Port != 8081 {
		t.Errorf("ONVIF.Port = %d", cfg.ONVIF.Port)
	}
	if cfg.ONVIF.Username != "onvifuser" {
		t.Errorf("ONVIF.Username = %q", cfg.ONVIF.Username)
	}
	if cfg.ONVIF.Password != "onvifpass" {
		t.Errorf("ONVIF.Password = %q", cfg.ONVIF.Password)
	}

	// Device
	if cfg.Device.Name != "Test Camera" {
		t.Errorf("Device.Name = %q", cfg.Device.Name)
	}
	if cfg.Device.Manufacturer != "TestCorp" {
		t.Errorf("Device.Manufacturer = %q", cfg.Device.Manufacturer)
	}
	if cfg.Device.Model != "TC-1000" {
		t.Errorf("Device.Model = %q", cfg.Device.Model)
	}
	if cfg.Device.Firmware != "2.0.0" {
		t.Errorf("Device.Firmware = %q", cfg.Device.Firmware)
	}
	if cfg.Device.HardwareID != "TC1000" {
		t.Errorf("Device.HardwareID = %q", cfg.Device.HardwareID)
	}
	if cfg.Device.SerialNumber != "SN-001" {
		t.Errorf("Device.SerialNumber = %q", cfg.Device.SerialNumber)
	}

	// Logging
	if cfg.Logging.Level != "debug" {
		t.Errorf("Logging.Level = %q", cfg.Logging.Level)
	}
}

func TestEnvOverride(t *testing.T) {
	// Set env vars before loading
	t.Setenv("MIBEE_EYE_CAMERA_WIDTH", "640")
	t.Setenv("MIBEE_EYE_CAMERA_HEIGHT", "480")
	t.Setenv("MIBEE_EYE_CAMERA_FPS", "30")
	t.Setenv("MIBEE_EYE_CAMERA_BRIGHTNESS", "0.5")
	t.Setenv("MIBEE_EYE_CAMERA_CONTRAST", "2.5")
	t.Setenv("MIBEE_EYE_CAMERA_SATURATION", "1.2")
	t.Setenv("MIBEE_EYE_CAMERA_SHARPNESS", "0.8")
	t.Setenv("MIBEE_EYE_RTSP_PORT", "9554")
	t.Setenv("MIBEE_EYE_RTSP_USERNAME", "envuser")
	t.Setenv("MIBEE_EYE_RTSP_PASSWORD", "envpass")
	t.Setenv("MIBEE_EYE_ONVIF_PORT", "9080")
	t.Setenv("MIBEE_EYE_ONVIF_USERNAME", "envonvif")
	t.Setenv("MIBEE_EYE_ONVIF_PASSWORD", "envonvifpass")
	t.Setenv("MIBEE_EYE_CAMERA_DEVICE", "/dev/videoEnv")
	t.Setenv("MIBEE_EYE_CAMERA_CODEC", "h265")
	t.Setenv("MIBEE_EYE_CAMERA_BITRATE", "5000000")
	t.Setenv("MIBEE_EYE_DEVICE_NAME", "Env Camera")
	t.Setenv("MIBEE_EYE_DEVICE_MANUFACTURER", "EnvCorp")
	t.Setenv("MIBEE_EYE_DEVICE_MODEL", "Env-2000")
	t.Setenv("MIBEE_EYE_DEVICE_FIRMWARE", "3.0.0")
	t.Setenv("MIBEE_EYE_DEVICE_HARDWAREID", "ENV2000")
	t.Setenv("MIBEE_EYE_DEVICE_SERIALNUMBER", "ENV-001")
	t.Setenv("MIBEE_EYE_LOGGING_LEVEL", "debug")

	// Load a config with different YAML values to prove env wins
	cfgYAML := `
camera:
  width: 999
  device: /dev/videoYAML
rtsp:
  port: 1111
onvif:
  username: "yamluser"
device:
  name: "YAML Camera"
logging:
  level: "warn"
`
	path := writeTempYAML(t, cfgYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Env overrides take precedence over YAML
	if cfg.Camera.Width != 640 {
		t.Errorf("Camera.Width = %d, want 640 (env override)", cfg.Camera.Width)
	}
	if cfg.Camera.Device != "/dev/videoEnv" {
		t.Errorf("Camera.Device = %q, want /dev/videoEnv (env override)", cfg.Camera.Device)
	}
	if cfg.Camera.Height != 480 {
		t.Errorf("Camera.Height = %d, want 480 (env override)", cfg.Camera.Height)
	}
	if cfg.Camera.FPS != 30 {
		t.Errorf("Camera.FPS = %d, want 30 (env override)", cfg.Camera.FPS)
	}
	if cfg.Camera.Codec != "h265" {
		t.Errorf("Camera.Codec = %q, want h265 (env override)", cfg.Camera.Codec)
	}
	if cfg.Camera.Bitrate != 5_000_000 {
		t.Errorf("Camera.Bitrate = %d, want 5000000 (env override)", cfg.Camera.Bitrate)
	}
	if cfg.Camera.Brightness != 0.5 {
		t.Errorf("Camera.Brightness = %f, want 0.5 (env override)", cfg.Camera.Brightness)
	}
	if cfg.Camera.Contrast != 2.5 {
		t.Errorf("Camera.Contrast = %f, want 2.5 (env override)", cfg.Camera.Contrast)
	}
	if cfg.Camera.Saturation != 1.2 {
		t.Errorf("Camera.Saturation = %f, want 1.2 (env override)", cfg.Camera.Saturation)
	}
	if cfg.Camera.Sharpness != 0.8 {
		t.Errorf("Camera.Sharpness = %f, want 0.8 (env override)", cfg.Camera.Sharpness)
	}
	if cfg.RTSP.Port != 9554 {
		t.Errorf("RTSP.Port = %d, want 9554 (env override)", cfg.RTSP.Port)
	}
	if cfg.RTSP.Username != "envuser" {
		t.Errorf("RTSP.Username = %q, want envuser (env override)", cfg.RTSP.Username)
	}
	if cfg.RTSP.Password != "envpass" {
		t.Errorf("RTSP.Password = %q, want envpass (env override)", cfg.RTSP.Password)
	}
	if cfg.ONVIF.Port != 9080 {
		t.Errorf("ONVIF.Port = %d, want 9080 (env override)", cfg.ONVIF.Port)
	}
	if cfg.ONVIF.Username != "envonvif" {
		t.Errorf("ONVIF.Username = %q, want envonvif (env override)", cfg.ONVIF.Username)
	}
	if cfg.ONVIF.Password != "envonvifpass" {
		t.Errorf("ONVIF.Password = %q, want envonvifpass (env override)", cfg.ONVIF.Password)
	}
	if cfg.Device.Name != "Env Camera" {
		t.Errorf("Device.Name = %q, want Env Camera (env override)", cfg.Device.Name)
	}
	if cfg.Device.Manufacturer != "EnvCorp" {
		t.Errorf("Device.Manufacturer = %q, want EnvCorp (env override)", cfg.Device.Manufacturer)
	}
	if cfg.Device.Model != "Env-2000" {
		t.Errorf("Device.Model = %q, want Env-2000 (env override)", cfg.Device.Model)
	}
	if cfg.Device.Firmware != "3.0.0" {
		t.Errorf("Device.Firmware = %q, want 3.0.0 (env override)", cfg.Device.Firmware)
	}
	if cfg.Device.HardwareID != "ENV2000" {
		t.Errorf("Device.HardwareID = %q, want ENV2000 (env override)", cfg.Device.HardwareID)
	}
	if cfg.Device.SerialNumber != "ENV-001" {
		t.Errorf("Device.SerialNumber = %q, want ENV-001 (env override)", cfg.Device.SerialNumber)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("Logging.Level = %q, want debug (env override)", cfg.Logging.Level)
	}
}

func TestInvalidYAML(t *testing.T) {
	cfgYAML := `camera: { invalid yaml: `
	path := writeTempYAML(t, cfgYAML)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestValidateNegativeFPS(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Camera.FPS = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative FPS, got nil")
	}
	if !strings.Contains(err.Error(), "fps") {
		t.Errorf("error should mention 'fps', got: %v", err)
	}
}

func TestValidateZeroPort(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ONVIF.Port = 0
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for zero ONVIF port, got nil")
	}
	if !strings.Contains(err.Error(), "onvif.port") {
		t.Errorf("error should mention 'onvif.port', got: %v", err)
	}
}

func TestValidateInvalidBrightness(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Camera.Brightness = 5.0
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for invalid brightness, got nil")
	}
	if !strings.Contains(err.Error(), "brightness") {
		t.Errorf("error should mention 'brightness', got: %v", err)
	}
}

func TestValidateValidConfig(t *testing.T) {
	cfg := DefaultConfig()
	err := cfg.Validate()
	if err != nil {
		t.Errorf("DefaultConfig() should be valid, got: %v", err)
	}
}

func TestValidateInvalidCodec(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Camera.Codec = "vp9"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for invalid codec, got nil")
	}
	if !strings.Contains(err.Error(), "codec") {
		t.Errorf("error should mention 'codec', got: %v", err)
	}
}

func TestLoadEmptyPasswordWarning(t *testing.T) {
	// Default config has empty ONVIF password; Load should succeed and log a warning.
	cfgYAML := `
camera: {}
rtsp: {}
onvif: {}
device: {}
logging: {}
`
	path := writeTempYAML(t, cfgYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed with empty password: %v", err)
	}
	if cfg.ONVIF.Password != "" {
		t.Errorf("expected empty password, got %q", cfg.ONVIF.Password)
	}
}

func TestGB28181Config_Defaults(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.GB28181.Enabled {
		t.Errorf("GB28181.Enabled = true, want false")
	}
	if cfg.GB28181.PlatformSIPAddress != "192.168.1.1" {
		t.Errorf("GB28181.PlatformSIPAddress = %q, want %q", cfg.GB28181.PlatformSIPAddress, "192.168.1.1")
	}
	if cfg.GB28181.PlatformSIPPort != 5060 {
		t.Errorf("GB28181.PlatformSIPPort = %d, want 5060", cfg.GB28181.PlatformSIPPort)
	}
	if cfg.GB28181.DeviceID != "34020000001320000001" {
		t.Errorf("GB28181.DeviceID = %q, want %q", cfg.GB28181.DeviceID, "34020000001320000001")
	}
	if cfg.GB28181.ChannelID != "34020000001320000001" {
		t.Errorf("GB28181.ChannelID = %q, want %q", cfg.GB28181.ChannelID, "34020000001320000001")
	}
	if cfg.GB28181.SIPDomain != "3402000000" {
		t.Errorf("GB28181.SIPDomain = %q, want %q", cfg.GB28181.SIPDomain, "3402000000")
	}
	if cfg.GB28181.Password != "" {
		t.Errorf("GB28181.Password = %q, want empty (no default password)", cfg.GB28181.Password)
	}
	if cfg.GB28181.LocalSIPPort != 5060 {
		t.Errorf("GB28181.LocalSIPPort = %d, want 5060", cfg.GB28181.LocalSIPPort)
	}
	if cfg.GB28181.RegisterIntervalSecs != 60 {
		t.Errorf("GB28181.RegisterIntervalSecs = %d, want 60", cfg.GB28181.RegisterIntervalSecs)
	}
	if cfg.GB28181.HeartbeatIntervalSecs != 60 {
		t.Errorf("GB28181.HeartbeatIntervalSecs = %d, want 60", cfg.GB28181.HeartbeatIntervalSecs)
	}
	if cfg.GB28181.HeartbeatTimeoutCount != 3 {
		t.Errorf("GB28181.HeartbeatTimeoutCount = %d, want 3", cfg.GB28181.HeartbeatTimeoutCount)
	}
}

func TestValidateGB28181PasswordRequired(t *testing.T) {
	cfg := DefaultConfig()
	cfg.GB28181.Enabled = true
	cfg.GB28181.Password = ""
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for empty gb28181 password when enabled, got nil")
	}
	if !strings.Contains(err.Error(), "gb28181.password") {
		t.Errorf("error should mention 'gb28181.password', got: %v", err)
	}

	// Disabled GB28181 with an empty password stays valid — no credential
	// is needed when the protocol is off.
	cfg.GB28181.Enabled = false
	if err := cfg.Validate(); err != nil {
		t.Errorf("disabled gb28181 with empty password should be valid, got: %v", err)
	}
}

func TestGB28181Config_EnvOverride(t *testing.T) {
	t.Setenv("MIBEE_EYE_GB28181_ENABLED", "true")
	t.Setenv("MIBEE_EYE_GB28181_PLATFORM_SIP_ADDRESS", "10.0.0.5")
	t.Setenv("MIBEE_EYE_GB28181_PLATFORM_SIP_PORT", "6060")
	t.Setenv("MIBEE_EYE_GB28181_DEVICE_ID", "34020000002000000002")
	t.Setenv("MIBEE_EYE_GB28181_CHANNEL_ID", "34020000002000000003")
	t.Setenv("MIBEE_EYE_GB28181_SIP_DOMAIN", "3402000001")
	t.Setenv("MIBEE_EYE_GB28181_PASSWORD", "envpass")
	t.Setenv("MIBEE_EYE_GB28181_LOCAL_SIP_PORT", "7060")
	t.Setenv("MIBEE_EYE_GB28181_REGISTER_INTERVAL_SECS", "120")
	t.Setenv("MIBEE_EYE_GB28181_HEARTBEAT_INTERVAL_SECS", "30")
	t.Setenv("MIBEE_EYE_GB28181_HEARTBEAT_TIMEOUT_COUNT", "5")

	// YAML values differ from env values to prove env wins
	cfgYAML := `
gb28181:
  platform_sip_address: "10.9.9.9"
  platform_sip_port: 9999
`
	path := writeTempYAML(t, cfgYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if !cfg.GB28181.Enabled {
		t.Errorf("GB28181.Enabled = false, want true (env override)")
	}
	if cfg.GB28181.PlatformSIPAddress != "10.0.0.5" {
		t.Errorf("GB28181.PlatformSIPAddress = %q, want 10.0.0.5 (env override)", cfg.GB28181.PlatformSIPAddress)
	}
	if cfg.GB28181.PlatformSIPPort != 6060 {
		t.Errorf("GB28181.PlatformSIPPort = %d, want 6060 (env override)", cfg.GB28181.PlatformSIPPort)
	}
	if cfg.GB28181.DeviceID != "34020000002000000002" {
		t.Errorf("GB28181.DeviceID = %q, want 34020000002000000002 (env override)", cfg.GB28181.DeviceID)
	}
	if cfg.GB28181.ChannelID != "34020000002000000003" {
		t.Errorf("GB28181.ChannelID = %q, want 34020000002000000003 (env override)", cfg.GB28181.ChannelID)
	}
	if cfg.GB28181.SIPDomain != "3402000001" {
		t.Errorf("GB28181.SIPDomain = %q, want 3402000001 (env override)", cfg.GB28181.SIPDomain)
	}
	if cfg.GB28181.Password != "envpass" {
		t.Errorf("GB28181.Password = %q, want envpass (env override)", cfg.GB28181.Password)
	}
	if cfg.GB28181.LocalSIPPort != 7060 {
		t.Errorf("GB28181.LocalSIPPort = %d, want 7060 (env override)", cfg.GB28181.LocalSIPPort)
	}
	if cfg.GB28181.RegisterIntervalSecs != 120 {
		t.Errorf("GB28181.RegisterIntervalSecs = %d, want 120 (env override)", cfg.GB28181.RegisterIntervalSecs)
	}
	if cfg.GB28181.HeartbeatIntervalSecs != 30 {
		t.Errorf("GB28181.HeartbeatIntervalSecs = %d, want 30 (env override)", cfg.GB28181.HeartbeatIntervalSecs)
	}
	if cfg.GB28181.HeartbeatTimeoutCount != 5 {
		t.Errorf("GB28181.HeartbeatTimeoutCount = %d, want 5 (env override)", cfg.GB28181.HeartbeatTimeoutCount)
	}

}

func TestGB28181Transport_Default(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.GB28181.Transport != "udp" {
		t.Errorf("GB28181.Transport = %q, want %q", cfg.GB28181.Transport, "udp")
	}
}

func TestGB28181Transport_EnvOverride(t *testing.T) {
	t.Setenv("MIBEE_EYE_GB28181_TRANSPORT", "tcp")

	// YAML value differs from env value to prove env wins
	cfgYAML := `
gb28181:
  transport: "udp"
`
	path := writeTempYAML(t, cfgYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.GB28181.Transport != "tcp" {
		t.Errorf("GB28181.Transport = %q, want tcp (env override)", cfg.GB28181.Transport)
	}
}

func TestGB28181Transport_InvalidRejected(t *testing.T) {
	t.Setenv("MIBEE_EYE_GB28181_TRANSPORT", "sctp")

	cfgYAML := `
camera: {}
rtsp: {}
onvif: {}
device: {}
logging: {}
`
	path := writeTempYAML(t, cfgYAML)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid transport, got nil")
	}
	if !strings.Contains(err.Error(), "transport") {
		t.Errorf("error should mention 'transport', got: %v", err)
	}
}

func TestAISectionDefaultsAndOverrides(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.AI.Enabled {
		t.Error("AI must be opt-in (default off)")
	}
	if cfg.AI.ModelPath == "" || cfg.AI.OnnxLibPath == "" || cfg.AI.DecoderBin == "" {
		t.Errorf("AI defaults incomplete: %+v", cfg.AI)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}

	cfgYAML := `
ai:
  enabled: true
  model_path: /opt/models/nanodet-m.onnx
  onnx_lib_path: /opt/lib/libonnxruntime.so
  confidence_threshold: 0.4
  interval_ms: 500
  decoder_bin: /usr/local/bin/ffmpeg
`
	path := writeTempYAML(t, cfgYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.AI.Enabled || cfg.AI.ModelPath != "/opt/models/nanodet-m.onnx" ||
		cfg.AI.OnnxLibPath != "/opt/lib/libonnxruntime.so" ||
		cfg.AI.ConfidenceThreshold != 0.4 || cfg.AI.IntervalMs != 500 ||
		cfg.AI.DecoderBin != "/usr/local/bin/ffmpeg" {
		t.Errorf("AI section misparsed: %+v", cfg.AI)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid AI section rejected: %v", err)
	}
}

func TestAISectionValidation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AI.Enabled = true
	cfg.AI.ModelPath = ""
	if err := cfg.Validate(); err == nil {
		t.Error("empty model_path must be rejected")
	}
	cfg = DefaultConfig()
	cfg.AI.Enabled = true
	cfg.AI.IntervalMs = 0
	if err := cfg.Validate(); err == nil {
		t.Error("zero interval_ms must be rejected")
	}
	cfg = DefaultConfig()
	cfg.AI.Enabled = true
	cfg.AI.ConfidenceThreshold = 1.5
	if err := cfg.Validate(); err == nil {
		t.Error("out-of-range confidence_threshold must be rejected")
	}
}

func TestGB35114ConfigParsing(t *testing.T) {
	cfgYAML := `
gb28181:
  enabled: true
  gb35114:
    enabled: true
    device_cert_file: "/etc/mibee-eye/gb35114/device_cert.pem"
    device_key_file: "/etc/mibee-eye/gb35114/device_key.pem"
    platform_cert_file: "/etc/mibee-eye/gb35114/platform_cert.pem"
    server_id: "34020000002000000001"
`
	path := writeTempYAML(t, cfgYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	g := cfg.GB28181.GB35114
	if !g.Enabled {
		t.Fatal("gb35114.enabled = false, want true")
	}
	if g.DeviceCertFile != "/etc/mibee-eye/gb35114/device_cert.pem" ||
		g.DeviceKeyFile != "/etc/mibee-eye/gb35114/device_key.pem" ||
		g.PlatformCertFile != "/etc/mibee-eye/gb35114/platform_cert.pem" {
		t.Fatalf("gb35114 cert paths not parsed: %+v", g)
	}
	if g.ServerID != "34020000002000000001" {
		t.Fatalf("gb35114.server_id = %q", g.ServerID)
	}
}

func TestGB35114ConfigDisabledByDefault(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, "gb28181:\n  enabled: true\n  password: \"12345678\"\n"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.GB28181.GB35114.Enabled {
		t.Fatal("gb35114 must default to disabled")
	}
}

func TestGB35114RelaxesPasswordRequirement(t *testing.T) {
	// GB35114 replaces Digest auth — an empty Digest password is valid
	// when A-level security is enabled.
	cfgYAML := `
gb28181:
  enabled: true
  gb35114:
    enabled: true
    server_id: "34020000002000000001"
`
	if _, err := Load(writeTempYAML(t, cfgYAML)); err != nil {
		t.Fatalf("Load with gb35114 and no password failed: %v", err)
	}
}
