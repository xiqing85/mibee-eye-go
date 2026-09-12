package onvifgo

import (
	"fmt"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/camera"
	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/config"

	"github.com/mickeyzzc/onvif-go/v2/server/provider"
)

// deviceInfoProvider supplies GetDeviceInformation from the service config.
type deviceInfoProvider struct {
	cfg *config.Config
}

func (p *deviceInfoProvider) DeviceInfo() provider.DeviceInfo {
	return provider.DeviceInfo{
		Manufacturer:    p.cfg.Device.Manufacturer,
		Model:           p.cfg.Device.Model,
		FirmwareVersion: p.cfg.Device.Firmware,
		SerialNumber:    p.cfg.Device.SerialNumber,
		HardwareID:      p.cfg.Device.HardwareID,
	}
}

// streamProvider answers GetStreamUri with the single RTSP mount: the
// path plus the configured port (#34, upstream v2.0.0-rc3). The host is
// derived by the library from AdvertiseHost — the device's own IP, like
// every other advertised URL.
type streamProvider struct {
	rtspPort int
}

func newStreamProvider(rtspPort int) *streamProvider {
	return &streamProvider{rtspPort: rtspPort}
}

// Stream implements provider.StreamURIProvider. The profile token is
// accepted as-is: the NVR echoes the token from GetProfiles, but the URI
// is always this service's single RTSP mount.
func (p *streamProvider) Stream(profileToken string) (provider.StreamInfo, error) {
	_ = profileToken
	return provider.StreamInfo{
		RTSPPath: "/stream",
		RTSPPort: p.rtspPort,
	}, nil
}

// imagingProvider bridges the Imaging service to camera.ParamManager.
// ONVIF PascalCase names map to camera params inside ParamManager; the
// AUTO/MANUAL enums translate to the camera's mode strings here.
type imagingProvider struct {
	pm *camera.ParamManager
}

// ImagingSettings implements provider.ImagingProvider.
func (p *imagingProvider) ImagingSettings(videoSourceToken string) (*provider.ImagingSettings, error) {
	_ = videoSourceToken // single video source; token accepted as-is

	brightness, err := p.pm.Get("Brightness")
	if err != nil {
		return nil, fmt.Errorf("get brightness: %w", err)
	}
	contrast, err := p.pm.Get("Contrast")
	if err != nil {
		return nil, fmt.Errorf("get contrast: %w", err)
	}
	saturation, err := p.pm.Get("Saturation")
	if err != nil {
		return nil, fmt.Errorf("get saturation: %w", err)
	}
	sharpness, err := p.pm.Get("Sharpness")
	if err != nil {
		return nil, fmt.Errorf("get sharpness: %w", err)
	}
	exposureTime, err := p.pm.Get("ExposureTime")
	if err != nil {
		return nil, fmt.Errorf("get exposure time: %w", err)
	}

	// Camera exposure "custom" = ONVIF MANUAL, anything else = AUTO.
	exposureMode := "AUTO"
	if rawMode, err := p.pm.Get("ExposureMode"); err == nil {
		if mode, ok := rawMode.(string); ok && mode == "custom" {
			exposureMode = "MANUAL"
		}
	}

	// Camera AWB "auto" = ONVIF AUTO, anything else = MANUAL.
	wbMode := "AUTO"
	if rawMode, err := p.pm.Get("AWBMode"); err == nil {
		if mode, ok := rawMode.(string); ok && mode != "auto" {
			wbMode = "MANUAL"
		}
	}

	return &provider.ImagingSettings{
		Brightness:      floatPtr(toFloat(brightness)),
		Contrast:        floatPtr(toFloat(contrast)),
		ColorSaturation: floatPtr(toFloat(saturation)),
		Sharpness:       floatPtr(toFloat(sharpness)),
		Exposure: &provider.ExposureSettings20{
			Mode:         exposureMode,
			ExposureTime: floatPtr(toFloat(exposureTime)),
		},
		WhiteBalance: &provider.WhiteBalanceSettings20{
			Mode: wbMode,
		},
	}, nil
}

// SetImagingSettings implements provider.ImagingProvider. Unknown enum
// values are ignored (not rejected), matching the historical handler.
func (p *imagingProvider) SetImagingSettings(videoSourceToken string, settings *provider.ImagingSettings) error {
	_ = videoSourceToken

	if settings == nil {
		return nil
	}

	if settings.Brightness != nil {
		if err := p.pm.Set("Brightness", *settings.Brightness); err != nil {
			return err
		}
	}
	if settings.Contrast != nil {
		if err := p.pm.Set("Contrast", *settings.Contrast); err != nil {
			return err
		}
	}
	if settings.ColorSaturation != nil {
		if err := p.pm.Set("Saturation", *settings.ColorSaturation); err != nil {
			return err
		}
	}
	if settings.Sharpness != nil {
		if err := p.pm.Set("Sharpness", *settings.Sharpness); err != nil {
			return err
		}
	}
	if settings.Exposure != nil {
		switch settings.Exposure.Mode {
		case "MANUAL":
			if err := p.pm.Set("ExposureMode", "custom"); err != nil {
				return err
			}
			if settings.Exposure.ExposureTime != nil {
				if err := p.pm.Set("ExposureTime", *settings.Exposure.ExposureTime); err != nil {
					return err
				}
			}
		case "AUTO":
			if err := p.pm.Set("ExposureMode", "normal"); err != nil {
				return err
			}
		}
	}
	if settings.WhiteBalance != nil {
		switch settings.WhiteBalance.Mode {
		case "MANUAL":
			if err := p.pm.Set("AWBMode", "custom"); err != nil {
				return err
			}
		case "AUTO":
			if err := p.pm.Set("AWBMode", "auto"); err != nil {
				return err
			}
		}
	}

	return nil
}

// ImagingOptions implements provider.ImagingProvider. Ranges come from the
// camera's declared ParamRanges; white-balance options mirror the
// historical GetOptions response.
func (p *imagingProvider) ImagingOptions(videoSourceToken string) (*provider.ImagingOptions, error) {
	_ = videoSourceToken

	return &provider.ImagingOptions{
		Brightness:      paramFloatRange("Brightness"),
		Contrast:        paramFloatRange("Contrast"),
		ColorSaturation: paramFloatRange("Saturation"),
		Sharpness:       paramFloatRange("Sharpness"),
		Exposure: &provider.ExposureOptions{
			Mode:            []string{"AUTO", "MANUAL"},
			MinExposureTime: paramFloatRange("ExposureTime"),
			MaxExposureTime: paramFloatRange("ExposureTime"),
			MinGain:         paramFloatRange("Gain"),
			MaxGain:         paramFloatRange("Gain"),
		},
		WhiteBalance: &provider.WhiteBalanceOptions{
			Mode:   []string{"AUTO", "MANUAL"},
			YrGain: &provider.FloatRange{Min: 0, Max: 1},
			YbGain: &provider.FloatRange{Min: 0, Max: 1},
		},
	}, nil
}

// MoveFocus implements provider.ImagingProvider. This device has no focus
// hardware; the Move action is not registered on the SOAP handler, so this
// only satisfies the interface.
func (p *imagingProvider) MoveFocus(videoSourceToken string, focus *provider.FocusMove) error {
	_ = videoSourceToken
	_ = focus
	return fmt.Errorf("focus control not supported by this device")
}

// paramFloatRange builds a provider.FloatRange from the camera's declared
// ParamRanges entry, or nil when the parameter has no range.
func paramFloatRange(name string) *provider.FloatRange {
	r, ok := camera.ParamRanges[name]
	if !ok {
		return nil
	}
	return &provider.FloatRange{Min: r.Min, Max: r.Max}
}

func floatPtr(v float64) *float64 {
	return &v
}

// toFloat converts a ParamManager value to float64. ParamManager stores
// camera params as uint32/float32/string/bool; imaging values are numeric.
func toFloat(v interface{}) float64 {
	switch val := v.(type) {
	case float64:
		return val
	case float32:
		return float64(val)
	case int:
		return float64(val)
	case int64:
		return float64(val)
	case uint32:
		return float64(val)
	case uint64:
		return float64(val)
	default:
		return 0
	}
}

// deviceNameOrDefault returns the advertised friendly name with the
// historical fallback.
func deviceNameOrDefault(cfg *config.Config) string {
	if cfg.Device.Name != "" {
		return cfg.Device.Name
	}
	return "Pi Camera V1"
}

// hardwareIDOrDefault returns the advertised hardware id with the
// historical fallback.
func hardwareIDOrDefault(cfg *config.Config) string {
	if cfg.Device.HardwareID != "" {
		return cfg.Device.HardwareID
	}
	return "OV5647"
}
