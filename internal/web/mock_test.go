package web

// mockOnvifConfig implements config.ConfigProvider for tests.

type mockOnvifConfig struct {
	port     int
	username string
	password string
}

func (m *mockOnvifConfig) ONVIFPort() int             { return m.port }
func (m *mockOnvifConfig) ONVIFUsername() string      { return m.username }
func (m *mockOnvifConfig) ONVIFPassword() string      { return m.password }
func (m *mockOnvifConfig) RTSPPort() int              { return 8554 }
func (m *mockOnvifConfig) DeviceIP() string           { return "192.168.1.1" }
func (m *mockOnvifConfig) CameraDevice() string       { return "/dev/video0" }
func (m *mockOnvifConfig) CameraCodec() string        { return "h264" }
func (m *mockOnvifConfig) CameraBitrate() int         { return 2000000 }
func (m *mockOnvifConfig) CameraWidth() int           { return 1280 }
func (m *mockOnvifConfig) CameraHeight() int          { return 720 }
func (m *mockOnvifConfig) CameraFPS() int             { return 15 }
func (m *mockOnvifConfig) CameraHFlip() bool          { return false }
func (m *mockOnvifConfig) CameraVFlip() bool          { return false }
func (m *mockOnvifConfig) DeviceName() string         { return "Test Camera" }
func (m *mockOnvifConfig) DeviceManufacturer() string { return "Test Manufacturer" }
func (m *mockOnvifConfig) DeviceModel() string        { return "TestModel" }
func (m *mockOnvifConfig) DeviceFirmware() string     { return "1.0.0" }
func (m *mockOnvifConfig) DeviceHardwareID() string   { return "TEST001" }
func (m *mockOnvifConfig) DeviceSerialNumber() string { return "" }
func (m *mockOnvifConfig) LoggingLevel() string       { return "info" }
func (m *mockOnvifConfig) SnapshotEnabled() bool      { return true }
func (m *mockOnvifConfig) SnapshotQuality() int       { return 85 }
