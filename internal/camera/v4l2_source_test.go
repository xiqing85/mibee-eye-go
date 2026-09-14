package camera

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/xiqing85/mibee-eye-go/internal/h264"
	"github.com/xiqing85/mibee-eye-go/internal/v4l2"
)

// ── encoder selection ────────────────────────────────────────────────────

type fakeEncoder struct{ name string }

func (f *fakeEncoder) Encode(yuv []byte, pts uint64) error { return nil }
func (f *fakeEncoder) Close() error                        { return nil }
func (f *fakeEncoder) Name() string                        { return f.name }

// stuckCapture never yields frames; the pump parks on its error path.
type stuckCapture struct{ closed bool }

func (c *stuckCapture) ReadFrame() ([]byte, error) {
	time.Sleep(50 * time.Millisecond)
	return nil, io.EOF
}
func (c *stuckCapture) Close() { c.closed = true }

func newTestSource(probe func(string) (v4l2.ProbeResult, error), m2m func(string, uint32, uint32) (frameEncoder, error)) *V4L2Source {
	return NewV4L2Source(
		WithV4L2Device("/dev/null"),
		WithV4L2EncoderDevice("/dev/video11"),
		WithV4L2Params(DefaultParams()),
		WithV4L2FFmpegBin(""), // never spawn a real ffmpeg
		WithV4L2Probe(probe),
		WithV4L2M2M(m2m),
		WithV4L2Capture(func(path string, w, h uint32) (captureDevice, error) {
			return &stuckCapture{}, nil
		}),
	)
}

func TestV4L2SelectsHardwareWhenM2MCapable(t *testing.T) {
	s := newTestSource(
		func(string) (v4l2.ProbeResult, error) {
			return v4l2.ProbeResult{M2MCapable: true, Driver: "bcm2835-codec"}, nil
		},
		func(string, uint32, uint32) (frameEncoder, error) {
			return &fakeEncoder{name: "m2m-stub"}, nil
		},
	)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()
	if s.encoder.Name() != "m2m-stub" {
		t.Fatalf("expected m2m-stub encoder, got %s", s.encoder.Name())
	}
}

func TestV4L2FailsWhenNoEncoderAvailable(t *testing.T) {
	// ffmpeg disabled (empty bin) + no M2M → startup must fail loudly
	// rather than silently producing nothing.
	s := newTestSource(
		func(string) (v4l2.ProbeResult, error) {
			return v4l2.ProbeResult{}, io.ErrClosedPipe // "not present"
		},
		func(string, uint32, uint32) (frameEncoder, error) {
			return nil, io.ErrClosedPipe
		},
	)
	if err := s.Start(context.Background()); err == nil {
		s.Stop()
		t.Fatal("expected start error with no usable encoder")
	}
}

func TestV4L2ParamsRoundTrip(t *testing.T) {
	s := NewV4L2Source(WithV4L2Params(DefaultParams()))
	if err := s.SetParam("brightness", float32(0.5)); err != nil {
		t.Fatalf("SetParam: %v", err)
	}
	v, err := s.GetParam("brightness")
	if err != nil {
		t.Fatalf("GetParam: %v", err)
	}
	if f, ok := v.(float32); !ok || f != 0.5 {
		t.Fatalf("brightness = %v (%T)", v, v)
	}
}

// ── ffmpeg readLoop AU splitting ─────────────────────────────────────────

func nal(t byte, payload []byte) []byte {
	out := []byte{0, 0, 0, 1, t}
	return append(out, payload...)
}

func TestFFmpegReadLoopSplitsAccessUnits(t *testing.T) {
	// AU1: SPS+PPS+IDR, AU2: P, AU3: P — plus one trailing sentinel P
	// that stays pending at EOF (streaming contract: the final trailing
	// NALU is only resolved by more data, exactly like rpicam-vid).
	stream := bytes.NewBuffer(nil)
	stream.Write(nal(7, []byte{0x64, 0x00, 0x1f})) // SPS
	stream.Write(nal(8, []byte{0xea, 0xec}))       // PPS
	stream.Write(nal(5, []byte{0x88, 0x84}))       // IDR
	stream.Write(nal(1, []byte{0x11}))             // P (new AU)
	stream.Write(nal(1, []byte{0x22}))             // P (new AU)
	stream.Write(nal(1, []byte{0x33}))             // sentinel (stays pending)

	var got [][]h264.NALU
	var keys []bool
	e := &ffmpegEncoder{parser: h264.NewParser(), stdout: io.NopCloser(bytes.NewReader(stream.Bytes()))}
	e.wg.Add(1) // readLoop does wg.Done on return; pair it when driven directly
	e.readLoop(func(nalus []h264.NALU, key bool) {
		cp := make([]h264.NALU, len(nalus))
		copy(cp, nalus)
		got = append(got, cp)
		keys = append(keys, key)
	})

	if len(got) != 3 {
		t.Fatalf("expected 3 AUs, got %d", len(got))
	}
	if len(got[0]) != 3 { // SPS+PPS+IDR stay one AU
		t.Fatalf("AU0 nalu count = %d, want 3", len(got[0]))
	}
	if !keys[0] {
		t.Fatal("AU0 must be keyframe")
	}
	if keys[1] || keys[2] {
		t.Fatal("AU1/AU2 must not be keyframes")
	}
}

func TestFFmpegReadLoopHandlesPartialChunks(t *testing.T) {
	// Same stream byte-split at awkward offsets must still split right.
	full := bytes.NewBuffer(nil)
	full.Write(nal(7, []byte{1}))
	full.Write(nal(5, []byte{2}))
	full.Write(nal(1, []byte{3}))
	full.Write(nal(1, []byte{4})) // sentinel: keeps the previous one complete
	data := full.Bytes()

	e := &ffmpegEncoder{parser: h264.NewParser(), stdout: io.NopCloser(bytes.NewReader(data))}
	// Feed manually in 3-byte chunks by driving readLoop's inner logic
	// indirectly — readLoop reads whatever the reader yields; a
	// NopCloser over the full slice yields it at once, so instead assert
	// the helper functions handle partial input.
	pending := []byte{}
	var complete []h264.NALU
	for i := 0; i < len(data); i += 3 {
		end := min(i+3, len(data))
		pending = append(pending, data[i:end]...)
		var nalus []h264.NALU
		nalus, pending = extractCompleteNALUs(pending, h264.NewParser())
		complete = append(complete, nalus...)
	}
	if len(complete) < 3 {
		t.Fatalf("expected ≥3 complete NALUs across partial chunks, got %d", len(complete))
	}
	e.wg.Add(1)                            // pair readLoop's Done when driven directly
	e.readLoop(func([]h264.NALU, bool) {}) // drain, no panic
}
