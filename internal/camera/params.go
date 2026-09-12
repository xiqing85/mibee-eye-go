// Portions derived from MediaMTX (https://github.com/bluenviron/mediamtx)
// Original code Copyright (c) bluenviron, MIT License
//
// params.go defines camera parameters for the mtxrpicam subprocess.
// Adapted from MediaMTX's internal/staticsources/rpicamera/params.go

package camera

// Params holds all configurable parameters for the mtxrpicam subprocess.
// These are serialized and sent to the subprocess via the pipe protocol.
type Params struct {
	LogLevel              string
	CameraID              uint32
	Width                 uint32
	Height                uint32
	HFlip                 bool
	VFlip                 bool
	Brightness            float32
	Contrast              float32
	Saturation            float32
	Sharpness             float32
	Exposure              string
	AWB                   string
	AWBGainRed            float32
	AWBGainBlue           float32
	Denoise               string
	Shutter               uint32
	Metering              string
	Gain                  float32
	EV                    float32
	ROI                   string
	HDR                   bool
	TuningFile            string
	Mode                  string
	FPS                   float32
	AfMode                string
	AfRange               string
	AfSpeed               string
	LensPosition          float32
	AfWindow              string
	FlickerPeriod         uint32
	TextOverlayEnable     bool
	TextOverlay           string
	Codec                 string
	IDRPeriod             uint32
	Bitrate               uint32
	Profile               string
	Level                 string
}

// DefaultParams returns params with sensible defaults for OV5647 on RPi 3B.
func DefaultParams() Params {
	return Params{
		Width:         1280,
		Height:        720,
		FPS:           15,
		Codec:         "hardwareH264",
		IDRPeriod:     60,
		Contrast:      1.0,
		Saturation:    1.0,
		Sharpness:     1.0,
		Exposure:      "normal",
		AWB:           "auto",
		Metering:      "centre",
		Denoise:       "auto",
		Bitrate:       2000000,
		LogLevel:      "info",
		Profile:              "main",
		Level:                "4.1",
	}
}
