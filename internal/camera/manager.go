// manager.go provides ParamManager that validates ONVIF Imaging parameters
// against defined ranges before forwarding to the Camera interface.
// This is the bridge between ONVIF naming conventions and the camera subprocess.

package camera

import (
	"fmt"
	"sync"
)

// Range defines the valid minimum, maximum, and default for a parameter.
type Range struct {
	Min, Max, Default float64
}

// ParamRanges defines valid ranges for ONVIF camera parameters.
// Keys use ONVIF PascalCase naming convention.
var ParamRanges = map[string]Range{
	"Brightness":   {Min: -1.0, Max: 1.0, Default: 0.0},
	"Contrast":     {Min: 0.0, Max: 32.0, Default: 1.0},
	"Saturation":   {Min: 0.0, Max: 32.0, Default: 1.0},
	"Sharpness":    {Min: 0.0, Max: 16.0, Default: 1.0},
	"ExposureTime": {Min: 0, Max: 1000000, Default: 0},
	"Gain":         {Min: 1.0, Max: 16.0, Default: 1.0},
	"Width":        {Min: 64, Max: 2592, Default: 1280},
	"Height":       {Min: 64, Max: 1944, Default: 720},
	"FPS":          {Min: 1, Max: 30, Default: 15},
	"HFlip":        {Min: 0, Max: 1, Default: 0},
	"VFlip":        {Min: 0, Max: 1, Default: 0},
}

// ParamEnums defines allowed string values for enum-type parameters.
// Keys use ONVIF PascalCase naming convention.
var ParamEnums = map[string][]string{
	"AWBMode":      {"auto", "incandescent", "tungsten", "fluorescent", "daylight", "cloudy", "custom"},
	"ExposureMode": {"normal", "sport", "short", "long", "custom"},
}

// onvifToCam maps ONVIF PascalCase parameter names to the lowercase names
// expected by the Camera interface's SetParam/GetParam.
var onvifToCam = map[string]string{
	"Brightness":   "brightness",
	"Contrast":     "contrast",
	"Saturation":   "saturation",
	"Sharpness":    "sharpness",
	"ExposureTime": "shutter",
	"Gain":         "gain",
	"Width":        "width",
	"Height":       "height",
	"FPS":          "fps",
	"HFlip":        "hFlip",
	"VFlip":        "vFlip",
	"AWBMode":      "awbMode",
	"ExposureMode": "exposureMode",
}

// ParamManager manages camera parameter changes with range validation.
// It wraps a Camera instance and validates ranges before forwarding.
type ParamManager struct {
	mu       sync.RWMutex
	cam      Camera
	onChange func(name string, value interface{})
}

// NewParamManager creates a new parameter manager wrapping a Camera.
func NewParamManager(cam Camera) *ParamManager {
	return &ParamManager{cam: cam}
}
// SetOnChange registers a callback invoked after a successful Set.
// The callback is called outside the mutex lock to avoid deadlock if the
// callback re-enters ParamManager. Pass nil to clear.
func (pm *ParamManager) SetOnChange(fn func(name string, value interface{})) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.onChange = fn
}

// Set validates and applies a parameter change.
// 1. Validates value is within range (if range defined)
// 2. Maps ONVIF param name to camera param name
// 3. Calls cam.SetParam() to send to subprocess
func (pm *ParamManager) Set(name string, value interface{}) error {
	if err := pm.Validate(name, value); err != nil {
		return err
	}

	camName, ok := onvifToCam[name]
	if !ok {
		return fmt.Errorf("unknown parameter: %s", name)
	}

	pm.mu.Lock()
	err := pm.cam.SetParam(camName, value)
	cb := pm.onChange
	pm.mu.Unlock()

	if err != nil {
		return err
	}
	if cb != nil {
		cb(name, value)
	}
	return nil
}

// Get returns the current value of a parameter from the camera.
func (pm *ParamManager) Get(name string) (interface{}, error) {
	camName, ok := onvifToCam[name]
	if !ok {
		return nil, fmt.Errorf("unknown parameter: %s", name)
	}

	pm.mu.RLock()
	defer pm.mu.RUnlock()

	return pm.cam.GetParam(camName)
}

// Validate checks if a value is within valid range without applying it.
func (pm *ParamManager) Validate(name string, value interface{}) error {
	// Check string enum parameters first (they are not in ParamRanges)
	if enums, ok := ParamEnums[name]; ok {
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("parameter %s requires a string value, got %T", name, value)
		}
		for _, e := range enums {
			if s == e {
				return nil
			}
		}
		return fmt.Errorf("parameter %s value %q not in allowed values %v", name, s, enums)
	}

	r, ok := ParamRanges[name]
	if !ok {
		return fmt.Errorf("unknown parameter: %s", name)
	}

	fv, err := toFloat64(value)
	if err != nil {
		return fmt.Errorf("invalid value for %s: %w", name, err)
	}

	if fv < r.Min || fv > r.Max {
		return fmt.Errorf("parameter %s value %v out of range [%.1f, %.1f]", name, fv, r.Min, r.Max)
	}

	return nil
}
// toFloat64 converts interface{} values to float64 for range comparison.
func toFloat64(value interface{}) (float64, error) {
	switch v := value.(type) {
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	case float32:
		return float64(v), nil
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case int32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case uint32:
		return float64(v), nil
	case uint64:
		return float64(v), nil
	default:
		return 0, fmt.Errorf("unsupported type %T", value)
	}
}
