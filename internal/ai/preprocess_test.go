package ai

import (
	"math"
	"testing"
)

// 320×320 grey frame: every channel equals (128 - mean) / std.
func TestPreprocessNormalizationFormula(t *testing.T) {
	src := make([]byte, 320*320*3)
	for i := range src {
		src[i] = 128
	}
	out, err := preprocessRGB8(src, 320, 320, 320, 320)
	if err != nil {
		t.Fatalf("preprocess: %v", err)
	}
	if len(out) != 307200 {
		t.Fatalf("output size = %d, want 307200", len(out))
	}
	expectedB := (128.0 - mean[0]) / std[0]
	if math.Abs(float64(out[0]-expectedB)) > 0.001 {
		t.Errorf("B[0] = %f, want ≈%f", out[0], expectedB)
	}
	// Same cross-check constant as the raspi pipeline: (128-103.53)/57.375.
	if math.Abs(float64(expectedB)-0.4266) > 0.001 {
		t.Errorf("expectedB = %f, want ≈0.4266", expectedB)
	}
}

func TestPreprocessChannelOrderIsBGR(t *testing.T) {
	// Pure red pixels: B and G land below their means, R above.
	src := make([]byte, 2*2*3)
	for px := 0; px < 4; px++ {
		src[px*3] = 255 // R
	}
	out, err := preprocessRGB8(src, 2, 2, 2, 2)
	if err != nil {
		t.Fatalf("preprocess: %v", err)
	}
	if out[0] >= 0 || out[4] >= 0 || out[8] <= 0 {
		t.Errorf("channel order wrong: B=%f G=%f R=%f", out[0], out[4], out[8])
	}
}

func TestPreprocessShortInputRejected(t *testing.T) {
	if _, err := preprocessRGB8(make([]byte, 10), 320, 320, 320, 320); err == nil {
		t.Fatal("short input must be rejected")
	}
}

func TestPreprocessNearestNeighborPicksTopLeft(t *testing.T) {
	// 2×2 checkerboard → 1×1 samples the (0,0) pixel (white).
	src := make([]byte, 2*2*3)
	for i := range src {
		src[i] = 0
	}
	src[0], src[1], src[2] = 255, 255, 255
	out, err := preprocessRGB8(src, 2, 2, 1, 1)
	if err != nil {
		t.Fatalf("preprocess: %v", err)
	}
	for i, v := range out {
		if v <= 0 {
			t.Fatalf("plane %d = %f, want positive (white pixel sampled)", i, v)
		}
	}
}

func TestPreprocessVariousSizes(t *testing.T) {
	for _, c := range []struct{ w, h uint32 }{{640, 480}, {1280, 720}, {1920, 1080}} {
		src := make([]byte, int(c.w)*int(c.h)*3)
		out, err := preprocessRGB8(src, c.w, c.h, 320, 320)
		if err != nil {
			t.Fatalf("%dx%d: %v", c.w, c.h, err)
		}
		if len(out) != 307200 {
			t.Fatalf("%dx%d: output size = %d", c.w, c.h, len(out))
		}
	}
}
