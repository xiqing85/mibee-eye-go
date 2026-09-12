package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// CameraConfig holds camera capture settings.
type CameraConfig struct {
	Device          string        `yaml:"device"`            // Camera device path
	Mode            string        `yaml:"mode"`              // Capture mode: "mtxrpicam" (default), "rpicamvid", or "rtsp"
	RTSPURL         string        `yaml:"rtsp_url"`          // External RTSP URL when mode=rtsp
	Width           int           `yaml:"width"`             // Capture width in pixels
	Height          int           `yaml:"height"`            // Capture height in pixels
	FPS             int           `yaml:"fps"`               // Frames per second
	Codec           string        `yaml:"codec"`             // Video codec (h264)
	Bitrate         int           `yaml:"bitrate"`           // Target bitrate in bps
	Brightness      float64       `yaml:"brightness"`        // -1.0 to 1.0
	Contrast        float64       `yaml:"contrast"`          // 0.0 to 32.0
	Saturation      float64       `yaml:"saturation"`        // 0.0 to 32.0
	Sharpness       float64       `yaml:"sharpness"`         // 0.0 to 16.0
	IDRPeriod       int           `yaml:"idr_period"`        // Keyframe interval (1=every frame, 15=every 15th)
	BinPath         string        `yaml:"bin_path"`          // Path to mtxrpicam binary
	FrameBufferSize int           `yaml:"frame_buffer_size"` // Frame channel buffer capacity
	MaxBackoff      time.Duration `yaml:"max_backoff"`       // Max subprocess restart backoff
	HFlip           bool          `yaml:"hflip"`             // Device-level horizontal mirror (baked into the encoded stream)
	VFlip           bool          `yaml:"vflip"`             // Device-level vertical flip (upside-down mount compensation)
}

// RTSPConfig holds RTSP server settings.
type RTSPConfig struct {
	Port                 int    `yaml:"port"`                   // RTSP port
	Username             string `yaml:"username"`               // RTSP authentication username
	Password             string `yaml:"password"`               // RTSP authentication password
	SubscriberBufferSize int    `yaml:"subscriber_buffer_size"` // AUHub subscriber channel buffer
	WriteQueueSize       int    `yaml:"write_queue_size"`       // gortsplib write queue size
	EnableUDP            bool   `yaml:"enable_udp"`             // Enable UDP transport (default: true, needed for NVR clients)
	UDPRTPPort           int    `yaml:"udp_rtp_port"`           // UDP RTP port (default: 8000)
	UDPRTCPPort          int    `yaml:"udp_rtcp_port"`          // UDP RTCP port (default: 8001)
}

// ONVIFConfig holds ONVIF server settings.
type ONVIFConfig struct {
	Port     int    `yaml:"port"`     // ONVIF HTTP port
	Username string `yaml:"username"` // ONVIF WS-UsernameToken username
	Password string `yaml:"password"` // ONVIF WS-UsernameToken password
}

// WebConfig holds Web UI server settings.
// The web UI serves a single-page admin panel for ONVIF config and camera params.
// When Username/Password are empty, the web server reuses the ONVIF credentials.
type WebConfig struct {
	Enabled           bool          `yaml:"enabled"`
	Port              int           `yaml:"port"`
	Username          string        `yaml:"username"`
	Password          string        `yaml:"password"`
	AllowedOrigins    []string      `yaml:"allowed_origins"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
}

// DeviceConfig holds ONVIF device information.
type DeviceConfig struct {
	Name         string `yaml:"name"`          // Camera friendly name
	Manufacturer string `yaml:"manufacturer"`  // Device manufacturer
	Model        string `yaml:"model"`         // Device model
	Firmware     string `yaml:"firmware"`      // Firmware version
	HardwareID   string `yaml:"hardware_id"`   // Hardware identifier
	SerialNumber string `yaml:"serial_number"` // Device serial number
}

// LoggingConfig holds logging settings.
type LoggingConfig struct {
	Level string `yaml:"level"` // Log level (debug, info, warn, error)
}

// MetricsConfig holds Prometheus metrics exporter settings.
type MetricsConfig struct {
	Enabled bool `yaml:"enabled"` // Enable the metrics HTTP endpoint (default: true)
	Port    int  `yaml:"port"`    // Metrics HTTP server port (default: 9100)
}

// SnapshotConfig holds snapshot endpoint settings.
type SnapshotConfig struct {
	Enabled bool `yaml:"enabled"` // Enable the snapshot endpoint (default: true)
	Quality int  `yaml:"quality"` // JPEG quality 1-100 (default: 85, only used for rpicam-still)
}

// RTMPConfig holds RTMP push client settings.
type RTMPConfig struct {
	Enabled    bool   `yaml:"enabled"`     // Enable RTMP push (default: false)
	URL        string `yaml:"url"`         // RTMP server URL (e.g. rtmp://host:port/app/streamkey)
	MaxRetries int    `yaml:"max_retries"` // Max reconnection attempts (default: 10)
}

// HLSConfig holds HLS live streaming settings.
type HLSConfig struct {
	Enabled         bool          `yaml:"enabled"`          // Enable HLS server (default: false)
	SegmentDuration time.Duration `yaml:"segment_duration"` // Target segment duration (default: 2s)
}

type GB28181Config struct {
	Enabled               bool   `yaml:"enabled"`                 // Enable GB28181 registration (default: false)
	PlatformSIPAddress    string `yaml:"platform_sip_address"`    // SIP server (platform) address
	PlatformSIPPort       int    `yaml:"platform_sip_port"`       // SIP server (platform) port
	DeviceID              string `yaml:"device_id"`               // GB28181 device ID (20 digits)
	ChannelID             string `yaml:"channel_id"`              // GB28181 channel ID (20 digits)
	SIPDomain             string `yaml:"sip_domain"`              // GB28181 SIP domain
	Password              string `yaml:"password"`                // SIP authentication password
	LocalSIPPort          int    `yaml:"local_sip_port"`          // Local SIP listening port
	RegisterIntervalSecs  int    `yaml:"register_interval_secs"`  // SIP REGISTER interval (seconds)
	HeartbeatIntervalSecs int    `yaml:"heartbeat_interval_secs"` // SIP keepalive heartbeat interval (seconds)
	HeartbeatTimeoutCount int    `yaml:"heartbeat_timeout_count"` // Missed heartbeats before declaring timeout
	Transport             string `yaml:"transport"`               // SIP transport: udp (default) or tcp

	// GB35114 opts into GB 35114 A-level security for the REGISTER
	// lifecycle. Requires a binary built with `-tags gb35114` and
	// pre-provisioned SM2 certificates (GM/T 0015-2012). Default off.
	GB35114 GB35114Config `yaml:"gb35114"`
}

// GB35114Config holds GB 35114 A-level security settings. When enabled,
// REGISTER authentication switches from SIP Digest to SM2-certificate
// mutual authentication; a build without `-tags gb35114` logs a warning
// and falls back to Digest.
type GB35114Config struct {
	Enabled          bool   `yaml:"enabled"`            // Enable A-level authentication (requires -tags gb35114)
	DeviceCertFile   string `yaml:"device_cert_file"`   // Device SM2 signing certificate (PEM)
	DeviceKeyFile    string `yaml:"device_key_file"`    // Device SM2 private key (SEC1 or PKCS#8 PEM)
	PlatformCertFile string `yaml:"platform_cert_file"` // Platform signing certificate — verifies sign2 (Bidirection)
	ServerID         string `yaml:"server_id"`          // 20-digit SIP server ID being authenticated to
}

// RecordingConfig holds local recording settings.
type RecordingConfig struct {
	Enabled       bool   `yaml:"enabled"`        // Enable continuous local recording (default: false)
	StoragePath   string `yaml:"storage_path"`   // Recording root directory (default: recordings)
	SegmentSecs   int    `yaml:"segment_secs"`   // Target segment duration in seconds (default: 600)
	RetentionDays int    `yaml:"retention_days"` // Delete segments older than this many days (0 = infinite)
	MaxStorageMB  int    `yaml:"max_storage_mb"` // Prune oldest segments above this cap in MB (0 = unlimited)
}

// AIConfig configures on-device object detection (SPEC v1 §4.6). The
// binary must be built with `-tags ai` for inference to be available;
// otherwise the feature reports unavailable (fail-open).
type AIConfig struct {
	Enabled             bool    `yaml:"enabled"`              // master switch (default off)
	AllowUpload         bool    `yaml:"allow_upload"`         // runtime model uploads (SPEC §4.6 ai_upload; default off)
	Model               string  `yaml:"model"`                // registry id of the startup model (SPEC §4.6)
	ModelPath           string  `yaml:"model_path"`           // NanoDet ONNX model (non-default overrides the registry id)
	OnnxLibPath         string  `yaml:"onnx_lib_path"`        // libonnxruntime.so location
	ConfidenceThreshold float32 `yaml:"confidence_threshold"` // report filter (0..1)
	IntervalMs          uint64  `yaml:"interval_ms"`          // min spacing between inferences
	DecoderBin          string  `yaml:"decoder_bin"`          // ffmpeg binary for keyframe decode
}

// Config is the top-level configuration for MiBee Eye.
type Config struct {
	Camera    CameraConfig    `yaml:"camera"`
	RTSP      RTSPConfig      `yaml:"rtsp"`
	ONVIF     ONVIFConfig     `yaml:"onvif"`
	Device    DeviceConfig    `yaml:"device"`
	Logging   LoggingConfig   `yaml:"logging"`
	Web       WebConfig       `yaml:"web"`
	Metrics   MetricsConfig   `yaml:"metrics"`
	Snapshot  SnapshotConfig  `yaml:"snapshot"`
	RTMP      RTMPConfig      `yaml:"rtmp"`
	HLS       HLSConfig       `yaml:"hls"`
	GB28181   GB28181Config   `yaml:"gb28181"`
	Recording RecordingConfig `yaml:"recording"`
	AI        AIConfig        `yaml:"ai"`
}

// DefaultConfig returns a Config with all default values.
func DefaultConfig() *Config {
	return &Config{
		Camera: CameraConfig{
			Device:          "/dev/video0",
			Mode:            "mtxrpicam",
			RTSPURL:         "",
			Width:           1280,
			Height:          720,
			FPS:             15,
			Codec:           "h264",
			Bitrate:         2_000_000,
			Brightness:      0.0,
			Contrast:        1.0,
			Saturation:      1.0,
			Sharpness:       1.0,
			IDRPeriod:       15,
			BinPath:         "deploy/bin/mtxrpicam",
			FrameBufferSize: 30,
			MaxBackoff:      30 * time.Second,
			HFlip:           false,
			VFlip:           false,
		},
		RTSP: RTSPConfig{
			Port:                 8554,
			Username:             "",
			Password:             "",
			SubscriberBufferSize: 64,
			WriteQueueSize:       2048,
			EnableUDP:            true,
			UDPRTPPort:           8000,
			UDPRTCPPort:          8001,
		},
		ONVIF: ONVIFConfig{
			Port:     8080,
			Username: "admin",
			Password: "",
		},
		Device: DeviceConfig{
			Name:         "Pi Camera V1",
			Manufacturer: "Raspberry Pi",
			Model:        "OV5647",
			Firmware:     "1.0.0",
			HardwareID:   "OV5647",
			SerialNumber: "",
		},
		Logging: LoggingConfig{
			Level: "info",
		},
		Web: WebConfig{
			Enabled:           true,
			Port:              8088,
			AllowedOrigins:    []string{"*"},
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       120 * time.Second,
		},
		Metrics: MetricsConfig{
			Enabled: true,
			Port:    9100,
		},
		Snapshot: SnapshotConfig{
			Enabled: true,
		},
		RTMP: RTMPConfig{
			Enabled:    false,
			URL:        "",
			MaxRetries: 10,
		},
		HLS: HLSConfig{
			Enabled:         false,
			SegmentDuration: 2 * time.Second,
		},
		GB28181: GB28181Config{
			Enabled:               false,
			PlatformSIPAddress:    "192.168.1.1",
			PlatformSIPPort:       5060,
			DeviceID:              "34020000001320000001",
			ChannelID:             "34020000001320000001",
			SIPDomain:             "3402000000",
			Password:              "",
			LocalSIPPort:          5060,
			RegisterIntervalSecs:  60,
			HeartbeatIntervalSecs: 60,
			HeartbeatTimeoutCount: 3,
			Transport:             "udp",
			GB35114:               GB35114Config{},
		},
		Recording: RecordingConfig{
			Enabled:       false,
			StoragePath:   "recordings",
			SegmentSecs:   600,
			RetentionDays: 3,
			MaxStorageMB:  8192,
		},
		AI: AIConfig{
			Enabled:             false,
			Model:               "nanodet-plus-m-320",
			ModelPath:           "/var/lib/mibee-eye/models/nanodet-m.onnx",
			OnnxLibPath:         "/usr/local/lib/libonnxruntime.so",
			ConfidenceThreshold: 0.35,
			IntervalMs:          1000,
			DecoderBin:          "ffmpeg",
		},
	}
}

// Load reads a YAML configuration file at path and returns a Config.
// Values from the file are merged over DefaultConfig().
// Environment variables with the MIBEE_EYE_ prefix override both.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	applyEnvOverrides(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	if cfg.ONVIF.Password == "" {
		slog.Warn("WARNING: ONVIF password is empty. Set onvif.password in config or MIBEE_EYE_ONVIF_PASSWORD env var")
	}

	return cfg, nil
}

// Environment variable names follow the pattern MIBEE_EYE_<SECTION>_<FIELD>.
func applyEnvOverrides(cfg *Config) {
	// Camera section
	overrideString("MIBEE_EYE_CAMERA_DEVICE", &cfg.Camera.Device)
	overrideInt("MIBEE_EYE_CAMERA_WIDTH", &cfg.Camera.Width)
	overrideInt("MIBEE_EYE_CAMERA_HEIGHT", &cfg.Camera.Height)
	overrideInt("MIBEE_EYE_CAMERA_FPS", &cfg.Camera.FPS)
	overrideString("MIBEE_EYE_CAMERA_CODEC", &cfg.Camera.Codec)
	overrideInt("MIBEE_EYE_CAMERA_BITRATE", &cfg.Camera.Bitrate)
	overrideFloat("MIBEE_EYE_CAMERA_BRIGHTNESS", &cfg.Camera.Brightness)
	overrideFloat("MIBEE_EYE_CAMERA_CONTRAST", &cfg.Camera.Contrast)
	overrideFloat("MIBEE_EYE_CAMERA_SATURATION", &cfg.Camera.Saturation)
	overrideFloat("MIBEE_EYE_CAMERA_SHARPNESS", &cfg.Camera.Sharpness)
	overrideInt("MIBEE_EYE_CAMERA_IDR_PERIOD", &cfg.Camera.IDRPeriod)
	overrideString("MIBEE_EYE_CAMERA_BIN_PATH", &cfg.Camera.BinPath)
	overrideInt("MIBEE_EYE_CAMERA_FRAME_BUFFER_SIZE", &cfg.Camera.FrameBufferSize)
	overrideDuration("MIBEE_EYE_CAMERA_MAX_BACKOFF", &cfg.Camera.MaxBackoff)
	// RTSP section
	overrideInt("MIBEE_EYE_RTSP_PORT", &cfg.RTSP.Port)
	overrideString("MIBEE_EYE_RTSP_USERNAME", &cfg.RTSP.Username)
	overrideString("MIBEE_EYE_RTSP_PASSWORD", &cfg.RTSP.Password)
	overrideInt("MIBEE_EYE_RTSP_SUBSCRIBER_BUFFER_SIZE", &cfg.RTSP.SubscriberBufferSize)
	overrideInt("MIBEE_EYE_RTSP_WRITE_QUEUE_SIZE", &cfg.RTSP.WriteQueueSize)
	overrideBool("MIBEE_EYE_RTSP_ENABLE_UDP", &cfg.RTSP.EnableUDP)
	overrideInt("MIBEE_EYE_RTSP_UDP_RTP_PORT", &cfg.RTSP.UDPRTPPort)
	overrideInt("MIBEE_EYE_RTSP_UDP_RTCP_PORT", &cfg.RTSP.UDPRTCPPort)
	// ONVIF section
	overrideInt("MIBEE_EYE_ONVIF_PORT", &cfg.ONVIF.Port)
	overrideString("MIBEE_EYE_ONVIF_USERNAME", &cfg.ONVIF.Username)
	overrideString("MIBEE_EYE_ONVIF_PASSWORD", &cfg.ONVIF.Password)
	// Web section
	overrideBool("MIBEE_EYE_WEB_ENABLED", &cfg.Web.Enabled)
	overrideInt("MIBEE_EYE_WEB_PORT", &cfg.Web.Port)
	overrideString("MIBEE_EYE_WEB_USERNAME", &cfg.Web.Username)
	overrideString("MIBEE_EYE_WEB_PASSWORD", &cfg.Web.Password)
	overrideStringSlice("MIBEE_EYE_WEB_ALLOWED_ORIGINS", &cfg.Web.AllowedOrigins)
	overrideDuration("MIBEE_EYE_WEB_READ_HEADER_TIMEOUT", &cfg.Web.ReadHeaderTimeout)
	overrideDuration("MIBEE_EYE_WEB_READ_TIMEOUT", &cfg.Web.ReadTimeout)
	overrideDuration("MIBEE_EYE_WEB_WRITE_TIMEOUT", &cfg.Web.WriteTimeout)
	overrideDuration("MIBEE_EYE_WEB_IDLE_TIMEOUT", &cfg.Web.IdleTimeout)
	// Metrics section
	overrideBool("MIBEE_EYE_METRICS_ENABLED", &cfg.Metrics.Enabled)
	overrideInt("MIBEE_EYE_METRICS_PORT", &cfg.Metrics.Port)
	// Device section
	overrideString("MIBEE_EYE_DEVICE_NAME", &cfg.Device.Name)
	overrideString("MIBEE_EYE_DEVICE_MANUFACTURER", &cfg.Device.Manufacturer)
	overrideString("MIBEE_EYE_DEVICE_MODEL", &cfg.Device.Model)
	overrideString("MIBEE_EYE_DEVICE_FIRMWARE", &cfg.Device.Firmware)
	overrideString("MIBEE_EYE_DEVICE_HARDWAREID", &cfg.Device.HardwareID)
	overrideString("MIBEE_EYE_DEVICE_SERIALNUMBER", &cfg.Device.SerialNumber)

	// Logging section
	overrideString("MIBEE_EYE_LOGGING_LEVEL", &cfg.Logging.Level)
	// Snapshot section
	overrideBool("MIBEE_EYE_SNAPSHOT_ENABLED", &cfg.Snapshot.Enabled)
	overrideInt("MIBEE_EYE_SNAPSHOT_QUALITY", &cfg.Snapshot.Quality)

	// RTMP section
	overrideBool("MIBEE_EYE_RTMP_ENABLED", &cfg.RTMP.Enabled)
	overrideString("MIBEE_EYE_RTMP_URL", &cfg.RTMP.URL)
	overrideInt("MIBEE_EYE_RTMP_MAX_RETRIES", &cfg.RTMP.MaxRetries)
	// HLS section
	overrideBool("MIBEE_EYE_HLS_ENABLED", &cfg.HLS.Enabled)
	overrideDuration("MIBEE_EYE_HLS_SEGMENT_DURATION", &cfg.HLS.SegmentDuration)
	// GB28181 section
	overrideBool("MIBEE_EYE_GB28181_ENABLED", &cfg.GB28181.Enabled)
	overrideString("MIBEE_EYE_GB28181_PLATFORM_SIP_ADDRESS", &cfg.GB28181.PlatformSIPAddress)
	overrideInt("MIBEE_EYE_GB28181_PLATFORM_SIP_PORT", &cfg.GB28181.PlatformSIPPort)
	overrideBool("MIBEE_EYE_GB28181_GB35114_ENABLED", &cfg.GB28181.GB35114.Enabled)
	overrideString("MIBEE_EYE_GB28181_GB35114_DEVICE_CERT_FILE", &cfg.GB28181.GB35114.DeviceCertFile)
	overrideString("MIBEE_EYE_GB28181_GB35114_DEVICE_KEY_FILE", &cfg.GB28181.GB35114.DeviceKeyFile)
	overrideString("MIBEE_EYE_GB28181_GB35114_PLATFORM_CERT_FILE", &cfg.GB28181.GB35114.PlatformCertFile)
	overrideString("MIBEE_EYE_GB28181_GB35114_SERVER_ID", &cfg.GB28181.GB35114.ServerID)
	overrideString("MIBEE_EYE_GB28181_DEVICE_ID", &cfg.GB28181.DeviceID)
	overrideString("MIBEE_EYE_GB28181_CHANNEL_ID", &cfg.GB28181.ChannelID)
	overrideString("MIBEE_EYE_GB28181_SIP_DOMAIN", &cfg.GB28181.SIPDomain)
	overrideString("MIBEE_EYE_GB28181_PASSWORD", &cfg.GB28181.Password)
	overrideInt("MIBEE_EYE_GB28181_LOCAL_SIP_PORT", &cfg.GB28181.LocalSIPPort)
	overrideInt("MIBEE_EYE_GB28181_REGISTER_INTERVAL_SECS", &cfg.GB28181.RegisterIntervalSecs)
	overrideInt("MIBEE_EYE_GB28181_HEARTBEAT_INTERVAL_SECS", &cfg.GB28181.HeartbeatIntervalSecs)
	overrideInt("MIBEE_EYE_GB28181_HEARTBEAT_TIMEOUT_COUNT", &cfg.GB28181.HeartbeatTimeoutCount)
	overrideString("MIBEE_EYE_GB28181_TRANSPORT", &cfg.GB28181.Transport)
	// Recording section
	overrideBool("MIBEE_EYE_RECORDING_ENABLED", &cfg.Recording.Enabled)
	overrideString("MIBEE_EYE_RECORDING_STORAGE_PATH", &cfg.Recording.StoragePath)
	overrideInt("MIBEE_EYE_RECORDING_SEGMENT_SECS", &cfg.Recording.SegmentSecs)
	overrideInt("MIBEE_EYE_RECORDING_RETENTION_DAYS", &cfg.Recording.RetentionDays)
	overrideInt("MIBEE_EYE_RECORDING_MAX_STORAGE_MB", &cfg.Recording.MaxStorageMB)
	// AI section
	overrideBool("MIBEE_EYE_AI_ENABLED", &cfg.AI.Enabled)
	overrideString("MIBEE_EYE_AI_MODEL_PATH", &cfg.AI.ModelPath)
	overrideString("MIBEE_EYE_AI_ONNX_LIB_PATH", &cfg.AI.OnnxLibPath)
	overrideFloat32("MIBEE_EYE_AI_CONFIDENCE_THRESHOLD", &cfg.AI.ConfidenceThreshold)
	overrideUint64("MIBEE_EYE_AI_INTERVAL_MS", &cfg.AI.IntervalMs)
	overrideString("MIBEE_EYE_AI_DECODER_BIN", &cfg.AI.DecoderBin)
}

// Sentinel errors for config validation.
var (
	errMustBePositive   = errors.New("must be positive")
	errInvalidCodec     = errors.New("codec must be h264 or h265")
	errInvalidLogLevel  = errors.New("level must be debug, info, warn, or error")
	errInvalidTransport = errors.New("gb28181 transport must be udp or tcp")
)

// Validate checks all config fields and returns a detailed error if any are invalid.
// It uses fmt.Errorf with %%w wrapping for the inner validation error.
func (c *Config) Validate() error {
	if c.Camera.FPS <= 0 {
		return fmt.Errorf("config.camera.fps: %w", errMustBePositive)
	}
	if c.Camera.Width <= 0 {
		return fmt.Errorf("config.camera.width: %w", errMustBePositive)
	}
	if c.Camera.Height <= 0 {
		return fmt.Errorf("config.camera.height: %w", errMustBePositive)
	}
	if c.Camera.Bitrate <= 0 {
		return fmt.Errorf("config.camera.bitrate: %w", errMustBePositive)
	}
	if c.Camera.Brightness < -1.0 || c.Camera.Brightness > 1.0 {
		return fmt.Errorf("config.camera.brightness: %w", fmt.Errorf("out of range [-1.0, 1.0]"))
	}
	if c.Camera.Contrast < 0.0 || c.Camera.Contrast > 32.0 {
		return fmt.Errorf("config.camera.contrast: %w", fmt.Errorf("out of range [0.0, 32.0]"))
	}
	if c.Camera.Saturation < 0.0 || c.Camera.Saturation > 32.0 {
		return fmt.Errorf("config.camera.saturation: %w", fmt.Errorf("out of range [0.0, 32.0]"))
	}
	if c.Camera.Sharpness < 0.0 || c.Camera.Sharpness > 16.0 {
		return fmt.Errorf("config.camera.sharpness: %w", fmt.Errorf("out of range [0.0, 16.0]"))
	}
	if c.Camera.Codec != "h264" && c.Camera.Codec != "h265" {
		return fmt.Errorf("config.camera.codec: %w", errInvalidCodec)
	}
	if c.Camera.IDRPeriod <= 0 {
		return fmt.Errorf("config.camera.idr_period: %w", errMustBePositive)
	}
	if c.Camera.FrameBufferSize <= 0 {
		return fmt.Errorf("config.camera.frame_buffer_size: %w", errMustBePositive)
	}
	if c.Camera.MaxBackoff <= 0 {
		return fmt.Errorf("config.camera.max_backoff: %w", errMustBePositive)
	}
	if c.RTSP.SubscriberBufferSize <= 0 {
		return fmt.Errorf("config.rtsp.subscriber_buffer_size: %w", errMustBePositive)
	}
	if c.RTSP.WriteQueueSize <= 0 {
		return fmt.Errorf("config.rtsp.write_queue_size: %w", errMustBePositive)
	}
	if c.Web.ReadHeaderTimeout < 0 {
		return fmt.Errorf("config.web.read_header_timeout: must not be negative")
	}
	if c.Web.ReadTimeout < 0 {
		return fmt.Errorf("config.web.read_timeout: must not be negative")
	}
	if c.Web.WriteTimeout < 0 {
		return fmt.Errorf("config.web.write_timeout: must not be negative")
	}
	if c.Web.IdleTimeout < 0 {
		return fmt.Errorf("config.web.idle_timeout: must not be negative")
	}
	if c.RTSP.Port <= 0 {
		return fmt.Errorf("config.rtsp.port: %w", errMustBePositive)
	}
	if c.ONVIF.Port <= 0 {
		return fmt.Errorf("config.onvif.port: %w", errMustBePositive)
	}
	if c.Web.Enabled && c.Web.Port <= 0 {
		return fmt.Errorf("config.web.port: %w", errMustBePositive)
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
		// valid
	default:
		return fmt.Errorf("config.logging.level: %w", errInvalidLogLevel)
	}
	if c.Metrics.Enabled && c.Metrics.Port <= 0 {
		return fmt.Errorf("config.metrics.port: %w", errMustBePositive)
	}
	if c.Snapshot.Enabled && (c.Snapshot.Quality < 0 || c.Snapshot.Quality > 100) {
		return fmt.Errorf("config.snapshot.quality: must be between 0 and 100, got %d", c.Snapshot.Quality)
	}
	switch c.GB28181.Transport {
	case "udp", "tcp":
		// valid
	case "":
		c.GB28181.Transport = "udp"
	default:
		return fmt.Errorf("config.gb28181.transport: %w", errInvalidTransport)
	}
	// GB35114 A-level replaces Digest auth, so the Digest password is not
	// required when it is enabled.
	if c.GB28181.Enabled && c.GB28181.Password == "" && !c.GB28181.GB35114.Enabled {
		return fmt.Errorf("config.gb28181.password: must not be empty when gb28181.enabled (set gb28181.password or MIBEE_EYE_GB28181_PASSWORD, or enable gb35114)")
	}
	if c.Recording.SegmentSecs < 60 {
		c.Recording.SegmentSecs = 60
	}
	if c.Recording.RetentionDays < 0 {
		return fmt.Errorf("config.recording.retention_days: must not be negative")
	}
	if c.Recording.MaxStorageMB < 0 {
		return fmt.Errorf("config.recording.max_storage_mb: must not be negative")
	}
	if c.AI.Enabled {
		if c.AI.ModelPath == "" {
			return fmt.Errorf("config.ai.model_path: must not be empty when ai.enabled")
		}
		if c.AI.IntervalMs == 0 {
			return fmt.Errorf("config.ai.interval_ms: %w", errMustBePositive)
		}
		if c.AI.ConfidenceThreshold <= 0 || c.AI.ConfidenceThreshold > 1 {
			return fmt.Errorf("config.ai.confidence_threshold: must be within (0, 1]")
		}
	}
	return nil
}

func overrideString(envName string, dest *string) {
	if v, ok := os.LookupEnv(envName); ok {
		*dest = v
	}
}

func overrideInt(envName string, dest *int) {
	if v, ok := os.LookupEnv(envName); ok {
		if n, err := strconv.Atoi(v); err == nil {
			*dest = n
		}
	}
}

func overrideFloat(envName string, dest *float64) {
	if v, ok := os.LookupEnv(envName); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			*dest = f
		}
	}
}

func overrideFloat32(envName string, dest *float32) {
	if v, ok := os.LookupEnv(envName); ok {
		if f, err := strconv.ParseFloat(v, 32); err == nil {
			*dest = float32(f)
		}
	}
}

func overrideUint64(envName string, dest *uint64) {
	if v, ok := os.LookupEnv(envName); ok {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			*dest = n
		}
	}
}

func overrideBool(envName string, dest *bool) {
	if v, ok := os.LookupEnv(envName); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			*dest = b
		}
	}
}

func overrideStringSlice(envName string, dest *[]string) {
	if v, ok := os.LookupEnv(envName); ok && v != "" {
		parts := strings.Split(v, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		*dest = parts
	}
}

func overrideDuration(envName string, dest *time.Duration) {
	if v, ok := os.LookupEnv(envName); ok {
		if d, err := time.ParseDuration(v); err == nil {
			*dest = d
		}
	}
}
