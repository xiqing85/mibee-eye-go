package main

import (
	"context"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/camera"
)

// noOpCamera is a stub camera.Camera used in tests.
// Imaging parameter changes are validated but not applied to hardware.
type noOpCamera struct{}

func (c *noOpCamera) Start(ctx context.Context) error               { return nil }
func (c *noOpCamera) Stop() error                                   { return nil }
func (c *noOpCamera) Frames() <-chan camera.Frame                   { return nil }
func (c *noOpCamera) SetParam(name string, value interface{}) error { return nil }
func (c *noOpCamera) GetParam(name string) (interface{}, error) {
	switch name {
	case "brightness":
		return 0.0, nil
	case "contrast":
		return 1.0, nil
	case "saturation":
		return 1.0, nil
	case "sharpness":
		return 1.0, nil
	case "exposure":
		return 0, nil
	case "gain":
		return 1.0, nil
	case "width":
		return 1280, nil
	case "height":
		return 720, nil
	case "fps":
		return 15, nil
	default:
		return nil, nil
	}
}
func (c *noOpCamera) Info() camera.CameraInfo {
	return camera.CameraInfo{}
}
