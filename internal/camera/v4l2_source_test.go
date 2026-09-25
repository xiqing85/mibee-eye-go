package camera

import (
	"bytes"
	"context"
	"io"
	"sync"
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

// ── device-level transform (SPEC appendix A #9/#19) ──────────────────────

// oneShotCapture yields a single prepared frame, then parks.
type oneShotCapture struct {
	frame  []byte
	closed bool
}

func (c *oneShotCapture) ReadFrame() ([]byte, error) {
	if c.frame != nil {
		f := c.frame
		c.frame = nil
		return f, nil
	}
	time.Sleep(50 * time.Millisecond)
	return nil, io.EOF
}
func (c *oneShotCapture) Close() { c.closed = true }

// captureEncoder records every frame it is asked to encode. The frame
// log is mutex-guarded: the pump goroutine writes it while tests poll.
type captureEncoder struct {
	name   string
	mu     sync.Mutex
	frames [][]byte
}

func (f *captureEncoder) Encode(yuv []byte, pts uint64) error {
	cp := make([]byte, len(yuv))
	copy(cp, yuv)
	f.mu.Lock()
	f.frames = append(f.frames, cp)
	f.mu.Unlock()
	return nil
}

// recordedFrames returns a copy of the frame log (race-safe polling).
func (f *captureEncoder) recordedFrames() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.frames...)
}
func (f *captureEncoder) Close() error { return nil }
func (f *captureEncoder) Name() string { return f.name }

// RequestKeyframe marks the encoder as IDR-capable (ForceIDR wiring tests).
func (f *captureEncoder) RequestKeyframe() error { return nil }

// pumpOneFrame drives the pump over a single prepared YU12 frame and
// returns what the encoder received.
func pumpOneFrame(t *testing.T, opts ...V4L2Option) []byte {
	t.Helper()
	params := DefaultParams()
	params.Width, params.Height = 3, 2 // matches frame3x2 plane layout
	all := append([]V4L2Option{
		WithV4L2Device("/dev/null"),
		WithV4L2EncoderDevice("/dev/video11"),
		WithV4L2Params(params),
		WithV4L2FFmpegBin(""),
		WithV4L2Probe(func(string) (v4l2.ProbeResult, error) {
			return v4l2.ProbeResult{M2MCapable: true}, nil
		}),
		WithV4L2Capture(func(string, uint32, uint32) (captureDevice, error) {
			return &oneShotCapture{frame: frame3x2()}, nil
		}),
	}, opts...)
	var m2mW, m2mH uint32
	enc := &captureEncoder{name: "capture-enc"}
	src := NewV4L2Source(append(all, WithV4L2M2M(func(_ string, w, h uint32) (frameEncoder, error) {
		m2mW, m2mH = w, h
		return enc, nil
	}))...)
	if err := src.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer src.Stop()
	deadline := time.Now().Add(2 * time.Second)
	var frames [][]byte
	for time.Now().Before(deadline) && len(frames) == 0 {
		time.Sleep(5 * time.Millisecond)
		frames = enc.recordedFrames()
	}
	if len(frames) == 0 {
		t.Fatal("pump never delivered a frame to the encoder")
	}
	t.Logf("m2m opened at %dx%d", m2mW, m2mH)
	return frames[0]
}

func TestV4L2PumpBakesRotation90(t *testing.T) {
	got := pumpOneFrame(t, WithV4L2Rotation(90))
	want := []byte{4, 1, 5, 2, 6, 3, 7, 8, 9, 10}
	if !bytes.Equal(got, want) {
		t.Fatalf("rotated frame = %v want %v", got, want)
	}
}

func TestV4L2PumpBakesRuntimeFlip(t *testing.T) {
	// Flips seeded from params reach the encoder in this backend (raw
	// pixels are ours) — and runtime SetParam updates them (next test).
	params := DefaultParams()
	params.Width, params.Height = 3, 2
	params.HFlip = true
	got := pumpOneFrame(t, WithV4L2Params(params))
	// Y [[1,2,3],[4,5,6]] hflip → [[3,2,1],[6,5,4]]; chroma rows mirrored.
	want := []byte{3, 2, 1, 6, 5, 4, 8, 7, 10, 9}
	if !bytes.Equal(got, want) {
		t.Fatalf("flipped frame = %v want %v", got, want)
	}
}

func TestV4L2PumpSetParamFlipTakesEffect(t *testing.T) {
	params := DefaultParams()
	params.Width, params.Height = 3, 2
	enc := &captureEncoder{name: "capture-enc"}
	src := NewV4L2Source(
		WithV4L2Device("/dev/null"),
		WithV4L2EncoderDevice("/dev/video11"),
		WithV4L2Params(params),
		WithV4L2FFmpegBin(""),
		WithV4L2Probe(func(string) (v4l2.ProbeResult, error) {
			return v4l2.ProbeResult{M2MCapable: true}, nil
		}),
		WithV4L2Capture(func(string, uint32, uint32) (captureDevice, error) {
			return &oneShotCapture{frame: frame3x2()}, nil
		}),
		WithV4L2M2M(func(string, uint32, uint32) (frameEncoder, error) {
			return enc, nil
		}),
	)
	if err := src.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer src.Stop()
	if !src.flipV.Load() {
		src.SetParam("vFlip", true)
	}
	if !src.flipV.Load() {
		t.Fatal("SetParam(vFlip) must flip the transform atomic")
	}
	v, err := src.GetParam("vFlip")
	if err != nil || v != true {
		t.Fatalf("GetParam(vFlip) = %v, %v", v, err)
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

// ── ForceIDR (DeviceControl IFrameCmd wiring) ────────────────────────────

type keyframeEncoder struct {
	fakeEncoder
	requests int
	err      error
}

func (k *keyframeEncoder) RequestKeyframe() error {
	k.requests++
	return k.err
}

// ForceIDR forwards to an encoder implementing RequestKeyframe.
func TestV4L2ForceIDRForwardsToEncoder(t *testing.T) {
	enc := &keyframeEncoder{fakeEncoder: fakeEncoder{name: "m2m-stub"}}
	s := newTestSource(
		func(string) (v4l2.ProbeResult, error) {
			return v4l2.ProbeResult{M2MCapable: true}, nil
		},
		func(string, uint32, uint32) (frameEncoder, error) { return enc, nil },
	)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()
	if err := s.ForceIDR(); err != nil {
		t.Fatalf("ForceIDR: %v", err)
	}
	if enc.requests != 1 {
		t.Fatalf("requests = %d", enc.requests)
	}
}

// ffmpeg-fallback encoders (no RequestKeyframe) report unsupported —
// the GB control logs once instead of failing the command silently.
func TestV4L2ForceIDRUnsupportedWithoutM2M(t *testing.T) {
	enc := &fakeEncoder{name: "ffmpeg-fallback"}
	s := newTestSource(
		func(string) (v4l2.ProbeResult, error) {
			return v4l2.ProbeResult{M2MCapable: true}, nil
		},
		func(string, uint32, uint32) (frameEncoder, error) { return enc, nil },
	)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()
	if err := s.ForceIDR(); err == nil {
		t.Fatal("ForceIDR must report unsupported")
	}
}
