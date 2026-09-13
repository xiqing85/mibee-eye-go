//go:build amd64 || arm64

package v4l2

import (
	"fmt"
	"unsafe"
)

// ProbeEncoder opens `path` and checks whether it is an M2M-capable node
// usable by [OpenM2MEncoder]. A missing node is reported as an error the
// caller can treat as "no hardware encoder".
func ProbeEncoder(path string) (ProbeResult, error) {
	file, err := openDevice(path)
	if err != nil {
		return ProbeResult{Path: path}, fmt.Errorf("v4l2: encoder node %s unavailable: %w", path, err)
	}
	defer file.Close()

	var caps V4l2Capability
	if err := ioctl(file.Fd(), vidiocQuerycap, unsafe.Pointer(&caps)); err != nil {
		return ProbeResult{Path: path}, fmt.Errorf("v4l2: QUERYCAP on %s: %w", path, err)
	}
	eff := caps.Capabilities
	if eff&0x80000000 != 0 && caps.DeviceCaps != 0 { // V4L2_CAP_DEVICE_CAPS
		eff = caps.DeviceCaps
	}
	m2m := eff&(CapVideoM2M|CapVideoM2MMplane) != 0 && eff&CapStreaming != 0
	return ProbeResult{Path: path, Driver: caps.DriverName(), M2MCapable: m2m}, nil
}
