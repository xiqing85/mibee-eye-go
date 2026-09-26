package camera

import "testing"

// frame8x4 builds an 8×4 I420 frame with distinct bytes per plane:
// Y = 1..=32, U = 0x10..0x17 (4×2), V = 0x20..0x27 (4×2).
func frame8x4() []byte {
	v := make([]byte, 0, 48)
	for i := 1; i <= 32; i++ {
		v = append(v, byte(i))
	}
	v = append(v, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17)
	v = append(v, 0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27)
	return v
}

func TestHalfScale8x4To4x2Golden(t *testing.T) {
	out := DownscaleYU12(frame8x4(), 8, 4, 4, 2)
	// Luma rows 0,2 × cols 0,2,4,6 (row 2 starts at byte 17).
	wantLuma := []byte{1, 3, 5, 7, 17, 19, 21, 23}
	for i, w := range wantLuma {
		if out[i] != w {
			t.Fatalf("luma[%d] = %d, want %d (out=%v)", i, out[i], w, out)
		}
	}
	// Chroma: src 4×2 → dst 2×1; nearest picks cols 0,2 of row 0.
	if out[8] != 0x10 || out[9] != 0x12 {
		t.Fatalf("U plane = %v, want [0x10 0x12]", out[8:10])
	}
	if out[10] != 0x20 || out[11] != 0x22 {
		t.Fatalf("V plane = %v, want [0x20 0x22]", out[10:12])
	}
	if len(out) != 4*2*3/2 {
		t.Fatalf("len = %d, want %d", len(out), 4*2*3/2)
	}
}

func TestExact2to1Golden(t *testing.T) {
	src := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 0xA1, 0xA2, 0xA3, 0xA4, 0xB1, 0xB2, 0xB3, 0xB4}
	out := DownscaleYU12(src, 4, 4, 2, 2)
	want := []byte{1, 3, 9, 11, 0xA1, 0xB1}
	for i, w := range want {
		if out[i] != w {
			t.Fatalf("out[%d] = %d, want %d (out=%v)", i, out[i], w, out)
		}
	}
}

func TestNonIntegerRatio(t *testing.T) {
	// 3×2 source: Y = [1,2,3 / 4,5,6], U = 0xAA, V = 0xBB.
	src := []byte{1, 2, 3, 4, 5, 6, 0xAA, 0xBB}
	out := DownscaleYU12(src, 3, 2, 2, 2)
	want := []byte{1, 2, 4, 5, 0xAA, 0xBB}
	for i, w := range want {
		if out[i] != w {
			t.Fatalf("out[%d] = %d, want %d (out=%v)", i, out[i], w, out)
		}
	}
}

func TestIdentityReturnsCopy(t *testing.T) {
	src := frame8x4()
	out := DownscaleYU12(src, 8, 4, 8, 4)
	for i := range src {
		if out[i] != src[i] {
			t.Fatalf("identity must copy verbatim")
		}
	}
}

func TestUpscaleIsDocumentedNoop(t *testing.T) {
	src := frame8x4()
	out := DownscaleYU12(src, 8, 4, 16, 8)
	if len(out) != len(src) {
		t.Fatalf("upscale must not attempt interpolation (len %d)", len(out))
	}
}

func TestShortInputReturnedUnchanged(t *testing.T) {
	short := []byte{1, 2, 3}
	out := DownscaleYU12(short, 8, 4, 4, 2)
	if len(out) != len(short) {
		t.Fatalf("short input must be returned unchanged")
	}
}

func Test720pToSubAllBytesPresent(t *testing.T) {
	src := make([]byte, 1280*720*3/2)
	for i := range src {
		src[i] = 7
	}
	out := DownscaleYU12(src, 1280, 720, 640, 360)
	if len(out) != 640*360*3/2 {
		t.Fatalf("len = %d, want %d", len(out), 640*360*3/2)
	}
}

func TestMapAxisNeverExceedsSource(t *testing.T) {
	m := mapAxis(359, 720)
	for _, x := range m {
		if x >= 720 {
			t.Fatalf("map index %d out of source range", x)
		}
	}
	if got := mapAxis(2, 4); got[0] != 0 || got[1] != 2 {
		t.Fatalf("mapAxis(2,4) = %v, want [0 2]", got)
	}
}
