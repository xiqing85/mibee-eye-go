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

func TestDownscaleYU12IntoMatchesAllocatingForm(t *testing.T) {
	src := make([]byte, 16*12*3/2)
	for i := range src {
		src[i] = byte(i * 5)
	}
	cases := [][4]uint32{{8, 6, 16, 12}, {16, 12, 16, 12}, {8, 6, 8, 12}, {20, 12, 16, 12}}
	for _, c := range cases {
		want := DownscaleYU12(src, 16, 12, c[0], c[1])
		got := DownscaleYU12Into(nil, src, 16, 12, c[0], c[1])
		if len(got) != len(want) {
			t.Fatalf("dims %dx%d: len %d, want %d", c[0], c[1], len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("dims %dx%d: byte %d = %d, want %d", c[0], c[1], i, got[i], want[i])
			}
		}
	}
}

func TestDownscaleYU12IntoReusesRoomyBuffer(t *testing.T) {
	src := make([]byte, 16*12*3/2)
	for i := range src {
		src[i] = byte(i)
	}
	buf := make([]byte, 256)
	got := DownscaleYU12Into(buf[:0], src, 16, 12, 8, 6)
	if &got[0] != &buf[0] {
		t.Fatal("roomy dst must be reused, not reallocated")
	}
	if len(got) != 8*6*3/2 {
		t.Fatalf("len = %d, want %d", len(got), 8*6*3/2)
	}
	// Never aliases src: mutating the result must leave src untouched.
	got[0] ^= 0xFF
	if src[0] == got[0] {
		t.Fatal("result must not alias the source buffer")
	}
}

func TestDownscaleYU12IntoGrowsTooSmallBuffer(t *testing.T) {
	src := make([]byte, 16*12*3/2)
	for i := range src {
		src[i] = byte(i)
	}
	buf := make([]byte, 4)
	got := DownscaleYU12Into(buf, src, 16, 12, 8, 6)
	if len(got) != 8*6*3/2 || cap(got) <= 4 {
		t.Fatalf("small dst must grow: len %d cap %d", len(got), cap(got))
	}
	want := DownscaleYU12(src, 16, 12, 8, 6)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byte %d = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestDownscaleYU12IntoIdentityPassThroughCopies(t *testing.T) {
	src := make([]byte, 16*12*3/2)
	for i := range src {
		src[i] = byte(i * 3)
	}
	got := DownscaleYU12Into(nil, src, 16, 12, 16, 12)
	if &got[0] == &src[0] {
		t.Fatal("identity path must copy, not alias src")
	}
	for i := range src {
		if got[i] != src[i] {
			t.Fatalf("identity byte %d = %d, want %d", i, got[i], src[i])
		}
	}
}
