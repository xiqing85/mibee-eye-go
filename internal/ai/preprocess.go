package ai

// RGB24 → resize → normalization preprocessing for NanoDet inference.
//
// Ported from mibee-eye-raspi-rs (identical semantics): BGR channel order,
// ImageNet mean/std applied to raw [0, 255] pixel values (no /255
// scaling), nearest-neighbor resize into the square model input (no
// letterboxing — x and y stretch independently), NCHW float32 output.

import "fmt"

// ImageNet-style normalization constants (BGR order, matching the NanoDet
// training config shared with the raspi cameras).
var (
	mean = [3]float32{103.53, 116.28, 123.675}
	std  = [3]float32{57.375, 57.12, 58.395}
)

// preprocessRGB8 converts an RGB8 frame to the model's NCHW input:
// nearest-neighbor resize to dstW x dstH, then (pixel-mean[c])/std[c] in
// BGR order, laid out channel-first ([B-plane, G-plane, R-plane]).
func preprocessRGB8(rgb []byte, srcW, srcH, dstW, dstH uint32) ([]float32, error) {
	if srcW == 0 || srcH == 0 || dstW == 0 || dstH == 0 {
		return nil, fmt.Errorf("ai: zero-sized frame (%dx%d → %dx%d)", srcW, srcH, dstW, dstH)
	}
	if uint64(len(rgb)) < uint64(srcW)*uint64(srcH)*3 {
		return nil, fmt.Errorf("ai: RGB frame too short: got %d bytes, need %d (%dx%dx3)",
			len(rgb), uint64(srcW)*uint64(srcH)*3, srcW, srcH)
	}

	scaleX := float32(srcW) / float32(dstW)
	scaleY := float32(srcH) / float32(dstH)

	plane := uint64(dstW) * uint64(dstH)
	output := make([]float32, 3*plane)

	for dy := uint32(0); dy < dstH; dy++ {
		sy := uint32(float32(dy) * scaleY)
		row := uint64(sy) * uint64(srcW)
		for dx := uint32(0); dx < dstW; dx++ {
			sx := uint32(float32(dx) * scaleX)
			idx := (row + uint64(sx)) * 3
			r, g, b := float32(rgb[idx]), float32(rgb[idx+1]), float32(rgb[idx+2])
			pixel := uint64(dy)*uint64(dstW) + uint64(dx)
			output[pixel] = (b - mean[0]) / std[0]
			output[plane+pixel] = (g - mean[1]) / std[1]
			output[2*plane+pixel] = (r - mean[2]) / std[2]
		}
	}
	return output, nil
}
