// Package config provides configuration loading and access for MiBee Eye.
//
// ConfigProvider is the unified configuration interface used by onvif and web
// packages to read camera, ONVIF, RTSP, and device settings.
package config

// ConfigProvider provides read-only access to camera, ONVIF, RTSP, and device
// configuration. This is the unified configuration interface used by the onvif
// and web packages. Implementations must provide all 18 methods.
type ConfigProvider interface {
	ONVIFUsername() string
	ONVIFPassword() string
	ONVIFPort() int
	RTSPPort() int
	DeviceIP() string
	CameraDevice() string
	CameraCodec() string
	CameraBitrate() int
	CameraWidth() int
	CameraHeight() int
	CameraFPS() int
	CameraHFlip() bool
	CameraVFlip() bool
	DeviceName() string
	DeviceManufacturer() string
	DeviceModel() string
	DeviceFirmware() string
	DeviceHardwareID() string
	DeviceSerialNumber() string
	LoggingLevel() string
	SnapshotEnabled() bool
	SnapshotQuality() int
}
