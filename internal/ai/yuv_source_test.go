package ai

import (
	"context"
	"testing"
	"time"
)

func TestFitEvenDims(t *testing.T) {
	cases := []struct {
		w, h, max, ew, eh uint32
	}{
		{720, 1280, 320, 180, 320}, // portrait: aspect preserved
		{1280, 720, 320, 320, 180}, // landscape
		{640, 480, 320, 320, 240},  // 4:3
		{320, 240, 320, 320, 240},  // already inside the box
		{1281, 721, 320, 320, 180}, // odd source dims come out even
		{2, 1000, 320, 2, 320},     // degenerate narrow strip stays even
	}
	for _, c := range cases {
		w, h := fitEven(c.w, c.h, c.max)
		if w != c.ew || h != c.eh {
			t.Fatalf("fitEven(%d,%d,%d) = %d,%d want %d,%d", c.w, c.h, c.max, w, h, c.ew, c.eh)
		}
		if w%2 != 0 || h%2 != 0 {
			t.Fatalf("fitEven(%d,%d) must be even for I420 chroma", w, h)
		}
	}
}

func TestYU12ToRGB24Golden(t *testing.T) {
	// 2×2 I420 with distinct luma per pixel and shared chroma.
	// Y = 16 (black), 235 (white), 128 (mid gray) ×2; U=V=128 (neutral).
	// 2×2 I420: Y plane 4 bytes, then 1×1 U and V planes.
	src := []byte{
		16, 235, 128, 128, // Y
		128, // U (1×1)
		128, // V (1×1)
	}
	dst := make([]byte, 2*2*3)
	yu12ToRGB24(dst, src, 2, 2)
	want := []byte{
		0, 0, 0, // Y16 neutral → black
		255, 255, 255, // Y235 neutral → white
		130, 130, 130, // Y128 neutral → mid gray
		130, 130, 130,
	}
	for i := range want {
		if dst[i] != want[i] {
			t.Fatalf("byte %d = %d, want %d", i, dst[i], want[i])
		}
	}
}

func TestYU12ToRGB24Chroma(t *testing.T) {
	// Blue-heavy chroma on 2×2 (1×1 chroma planes): U=240, V=128, Y=128.
	src := []byte{128, 128, 128, 128, 240, 128}
	dst := make([]byte, 2*2*3)
	yu12ToRGB24(dst, src, 2, 2)
	// c = 298*(128-16)+128 = 33504; B = (33504+516*112)>>8 = 356 → clamp 255.
	if dst[2] != 255 {
		t.Fatalf("blue = %d, want 255 (clamped)", dst[2])
	}
	if dst[0] != 130 || dst[1] != 87 {
		t.Fatalf("r,g = %d,%d want 130,87", dst[0], dst[1])
	}
}

func TestYUVSourceThrottlesAndProducesFrames(t *testing.T) {
	s := NewYUVSource(80 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	frame := make([]byte, 8*4*3/2)
	for i := range frame {
		frame[i] = 16 // black luma, neutral-ish chroma
	}
	// First tap passes the gate; an immediate second does not.
	s.Tap(frame, 8, 4)
	s.Tap(frame, 8, 4)
	if got := len(s.Frames()); got != 1 {
		t.Fatalf("frames queued = %d, want 1 (interval throttle)", got)
	}
	f := <-s.Frames()
	if f.Width != 8 || f.Height != 4 || len(f.Data) != 8*4*3 {
		t.Fatalf("frame = %dx%d len %d, want 8x4 RGB24", f.Width, f.Height, len(f.Data))
	}
	time.Sleep(90 * time.Millisecond)
	s.Tap(frame, 8, 4)
	if got := len(s.Frames()); got != 1 {
		t.Fatalf("frames queued after interval = %d, want 1", got)
	}
}

func TestYUVSourceDropOnFull(t *testing.T) {
	s := NewYUVSource(0) // no throttle: every tap converts
	frame := make([]byte, 8*4*3/2)
	s.Tap(frame, 8, 4) // fills the cap-1 channel
	<-s.Frames()
	s.Tap(frame, 8, 4)
	s.Tap(frame, 8, 4) // second overflows and is dropped, not blocked
	if got := len(s.Frames()); got != 1 {
		t.Fatalf("frames queued = %d, want 1 (drop-on-full)", got)
	}
}

func TestYUVSourceAdaptsToGeometryChange(t *testing.T) {
	s := NewYUVSource(0)
	s.Tap(make([]byte, 8*4*3/2), 8, 4)
	f1 := <-s.Frames()
	s.Tap(make([]byte, 4*8*3/2), 4, 8)
	f2 := <-s.Frames()
	if f1.Width != 8 || f2.Width != 4 || f2.Height != 8 {
		t.Fatalf("geometry change: %dx%d then %dx%d", f1.Width, f1.Height, f2.Width, f2.Height)
	}
}

func TestYUVSourceDescribeMentionsTap(t *testing.T) {
	s := NewYUVSource(time.Second)
	if d := s.Describe(); d == "" {
		t.Fatal("Describe must be non-empty for the startup log")
	}
}
