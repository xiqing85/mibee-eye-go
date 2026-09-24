package camera

import "testing"

// 3×2 YU12 frame: Y = 1..6, U = 7..8 (2×1), V = 9..10 (2×1).
// Plane sizes: y=6, chroma w=ceil(3/2)=2 × ceil(2/2)=1 → u=6..8, v=8..10.
func frame3x2() []byte {
	return []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
}

func TestRotatedDimsSwapOnlyFor90And270(t *testing.T) {
	cases := []struct {
		rotation, w, h, ew, eh int
	}{
		{0, 640, 480, 640, 480},
		{180, 640, 480, 640, 480},
		{90, 640, 480, 480, 640},
		{270, 640, 480, 480, 640},
		{45, 640, 480, 640, 480}, // out-of-enum behaves as 0
	}
	for _, c := range cases {
		if ew, eh := RotatedDims(c.w, c.h, c.rotation); ew != c.ew || eh != c.eh {
			t.Errorf("RotatedDims(%d,%d,%d) = %d,%d want %d,%d",
				c.w, c.h, c.rotation, ew, eh, c.ew, c.eh)
		}
	}
}

func TestRotate90CWTransposesPlanes(t *testing.T) {
	// Y [[1,2,3],[4,5,6]] --cw--> [[4,1],[5,2],[6,3]]
	// U row [7,8] --> column [7;8]; V row [9,10] --> column [9;10].
	buf := frame3x2()
	scratch := []byte{}
	w, h := RotateYU12(&buf, &scratch, 3, 2, 90)
	if w != 2 || h != 3 {
		t.Fatalf("dims = %d,%d want 2,3", w, h)
	}
	want := []byte{4, 1, 5, 2, 6, 3, 7, 8, 9, 10}
	for i, v := range want {
		if buf[i] != v {
			t.Fatalf("buf[%d] = %d want %d (full: %v)", i, buf[i], v, buf)
		}
	}
}

func TestRotate270CCWTransposesPlanes(t *testing.T) {
	// Y [[1,2,3],[4,5,6]] --ccw--> [[3,6],[2,5],[1,4]]
	// U row [7,8] --> column [8;7]; V row [9,10] --> column [10;9].
	buf := frame3x2()
	scratch := []byte{}
	w, h := RotateYU12(&buf, &scratch, 3, 2, 270)
	if w != 2 || h != 3 {
		t.Fatalf("dims = %d,%d want 2,3", w, h)
	}
	want := []byte{3, 6, 2, 5, 1, 4, 8, 7, 10, 9}
	for i, v := range want {
		if buf[i] != v {
			t.Fatalf("buf[%d] = %d want %d (full: %v)", i, buf[i], v, buf)
		}
	}
}

func TestRotate270IsInverseOfRotate90(t *testing.T) {
	original := frame3x2()
	buf := append([]byte(nil), original...)
	scratch := []byte{}
	w, h := RotateYU12(&buf, &scratch, 3, 2, 90)
	w2, h2 := RotateYU12(&buf, &scratch, w, h, 270)
	if w2 != 3 || h2 != 2 {
		t.Fatalf("dims = %d,%d want 3,2", w2, h2)
	}
	for i, v := range original {
		if buf[i] != v {
			t.Fatalf("round-trip buf[%d] = %d want %d", i, buf[i], v)
		}
	}
}

func TestRotate0And180AreNoopsHere(t *testing.T) {
	for _, rotation := range []int{0, 180} {
		buf := frame3x2()
		scratch := []byte{}
		w, h := RotateYU12(&buf, &scratch, 3, 2, rotation)
		if w != 3 || h != 2 {
			t.Fatalf("rotation %d: dims = %d,%d want 3,2", rotation, w, h)
		}
		for i, v := range frame3x2() {
			if buf[i] != v {
				t.Fatalf("rotation %d mutated buffer", rotation)
			}
		}
	}
}

func TestRotateShortBufferLeftUntouched(t *testing.T) {
	buf := []byte{7, 7, 7, 7}
	scratch := []byte{}
	w, h := RotateYU12(&buf, &scratch, 3, 2, 90)
	if w != 2 || h != 3 {
		t.Fatalf("dims = %d,%d want 2,3", w, h)
	}
	for _, b := range buf {
		if b != 7 {
			t.Fatal("short buffer mutated")
		}
	}
}

func TestComposeRotationFlipsFolds180(t *testing.T) {
	cases := []struct {
		rotation       int
		hf, vf         bool
		wantR          int
		wantHF, wantVF bool
	}{
		{180, false, false, 0, true, true},
		{180, true, false, 0, false, true},
		{180, false, true, 0, true, false},
		{180, true, true, 0, false, false},
		{90, true, false, 90, true, false},
		{270, false, true, 270, false, true},
		{45, true, true, 0, true, true},
	}
	for _, c := range cases {
		r, hf, vf := ComposeRotationFlips(c.rotation, c.hf, c.vf)
		if r != c.wantR || hf != c.wantHF || vf != c.wantVF {
			t.Errorf("Compose(%d,%v,%v) = %d,%v,%v want %d,%v,%v",
				c.rotation, c.hf, c.vf, r, hf, vf, c.wantR, c.wantHF, c.wantVF)
		}
	}
}

// 4×4 frame with distinct values per plane: Y = 0..15, U = 20..23, V = 30..33.
func frame4x4() []byte {
	buf := make([]byte, 24)
	for i := range buf {
		switch {
		case i < 16:
			buf[i] = byte(i)
		case i < 20:
			buf[i] = byte(20 + i - 16)
		default:
			buf[i] = byte(30 + i - 20)
		}
	}
	return buf
}

func TestFlipVflipReversesRowOrder(t *testing.T) {
	buf := frame4x4()
	FlipYU12(buf, 4, 4, false, true, nil)
	for r := 0; r < 4; r++ {
		for c := 0; c < 4; c++ {
			if got, want := buf[r*4+c], byte((3-r)*4+c); got != want {
				t.Fatalf("Y[%d,%d] = %d want %d", r, c, got, want)
			}
		}
	}
}

func TestFlipHflipMirrorsEachRow(t *testing.T) {
	buf := frame4x4()
	FlipYU12(buf, 4, 4, true, false, nil)
	for r := 0; r < 4; r++ {
		for c := 0; c < 4; c++ {
			if got, want := buf[r*4+c], byte(r*4+3-c); got != want {
				t.Fatalf("Y[%d,%d] = %d want %d", r, c, got, want)
			}
		}
	}
}

func TestFlipBothIsFullReversal(t *testing.T) {
	buf := frame4x4()
	FlipYU12(buf, 4, 4, true, true, nil)
	for i := 0; i < 16; i++ {
		if got, want := buf[i], byte(15-i); got != want {
			t.Fatalf("Y[%d] = %d want %d", i, got, want)
		}
	}
}

func TestFlipShortBufferLeftUntouched(t *testing.T) {
	buf := []byte{7, 7, 7, 7, 7, 7, 7, 7}
	FlipYU12(buf, 4, 4, true, true, nil)
	for _, b := range buf {
		if b != 7 {
			t.Fatal("short buffer mutated")
		}
	}
}

func TestNormalizeRotation(t *testing.T) {
	for in, want := range map[int]int{0: 0, 90: 90, 180: 180, 270: 270, 45: 0, -90: 0, 360: 0} {
		if got := NormalizeRotation(in); got != want {
			t.Errorf("NormalizeRotation(%d) = %d want %d", in, got, want)
		}
	}
}
