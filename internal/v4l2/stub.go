//go:build !(amd64 || arm64)

package v4l2

import "fmt"

// errUnsupported32 is the single error every stub returns: struct layouts
// in this package model the 64-bit V4L2 UABI only.
var errUnsupported32 = fmt.Errorf("v4l2: the v4l2 backend requires a 64-bit build (amd64/arm64)")

// ProbeEncoder reports unsupported on 32-bit targets.
func ProbeEncoder(path string) (ProbeResult, error) {
	return ProbeResult{Path: path}, errUnsupported32
}

// Capture is the 32-bit stub; see errUnsupported32.
type Capture struct{}

// ReadFrame always errors on 32-bit builds.
func (*Capture) ReadFrame() ([]byte, error) { return nil, errUnsupported32 }

// Close is a no-op on 32-bit builds.
func (*Capture) Close() {}

// OpenCapture reports unsupported on 32-bit targets.
func OpenCapture(path string, width, height uint32) (*Capture, error) {
	return nil, errUnsupported32
}

// M2MEncoder is the 32-bit stub; see errUnsupported32.
type M2MEncoder struct{}

// Encode always errors on 32-bit builds.
func (*M2MEncoder) Encode(yuv []byte) ([]byte, error) { return nil, errUnsupported32 }

// RequestKeyframe satisfies the 64-bit encoder's force-IDR seam on the
// 32-bit stub (the arm64 implementation issues
// V4L2_CID_MPEG_VIDEO_FORCE_KEY_FRAME; 32-bit M2M is unsupported anyway).
func (*M2MEncoder) RequestKeyframe() error { return errUnsupported32 }

// Close is a no-op on 32-bit builds.
func (*M2MEncoder) Close() {}

// OpenM2MEncoder reports unsupported on 32-bit targets.
func OpenM2MEncoder(path string, width, height uint32) (*M2MEncoder, error) {
	return nil, errUnsupported32
}
