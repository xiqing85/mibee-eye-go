package camera

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiqing85/mibee-eye-go/internal/h264"
	"github.com/xiqing85/mibee-eye-go/internal/v4l2"
)

// recordingEncoder captures the frames it is asked to encode.
type recordingEncoder struct {
	mu     sync.Mutex
	frames [][]byte
	done   chan struct{}
}

func (r *recordingEncoder) Encode(yuv []byte, pts uint64) error {
	buf := make([]byte, len(yuv))
	copy(buf, yuv)
	r.mu.Lock()
	r.frames = append(r.frames, buf)
	r.mu.Unlock()
	select {
	case r.done <- struct{}{}:
	default:
	}
	return nil
}
func (r *recordingEncoder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.frames)
}
func (r *recordingEncoder) frame(i int) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.frames[i]
}
func (r *recordingEncoder) Close() error { return nil }
func (r *recordingEncoder) Name() string { return "recording" }

func newTestPipeline(t *testing.T, o SubstreamOptions, enc frameEncoder) *SubstreamPipeline {
	t.Helper()
	o.ProbeEncoder = func(string) (v4l2.ProbeResult, error) {
		return v4l2.ProbeResult{M2MCapable: true}, nil
	}
	o.OpenM2M = func(string, uint32, uint32) (frameEncoder, error) { return enc, nil }
	p, err := NewSubstreamPipeline(o)
	if err != nil {
		t.Fatalf("NewSubstreamPipeline: %v", err)
	}
	return p
}

func TestSubstreamPipelineDownscalesAndDecimates(t *testing.T) {
	enc := &recordingEncoder{done: make(chan struct{}, 32)}
	// 8×4 main → 4×2 sub; main 10fps, sub 5fps → keep every 2nd frame.
	p := newTestPipeline(t, SubstreamOptions{
		SrcW: 8, SrcH: 4, Width: 4, Height: 2,
		FPs: 5, Bitrate: 400_000, MainFPS: 10,
	}, enc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Run(ctx)
	defer p.Stop()

	frame := frame8x4()
	for tag := byte(1); tag <= 4; tag++ {
		for i := range frame {
			if i < 32 {
				frame[i] = tag // luma tagged per frame
			}
		}
		p.Tap(frame, 8, 4)
		time.Sleep(20 * time.Millisecond) // let the pipeline consume each tap
	}

	// Wait for two encodes (tags 2 and 4), with a deadline.
	deadline := time.After(3 * time.Second)
	for enc.count() < 2 {
		select {
		case <-enc.done:
		case <-deadline:
			t.Fatalf("timed out waiting for encodes, got %d", enc.count())
		}
	}

	if got := len(enc.frame(0)); got != 4*2*3/2 {
		t.Fatalf("encoded frame len = %d, want %d (4x2 I420)", got, 4*2*3/2)
	}
	if enc.frame(0)[0] != 2 || enc.frame(1)[0] != 4 {
		t.Fatalf("decimation must keep frames 2 and 4, got tags %d/%d", enc.frame(0)[0], enc.frame(1)[0])
	}
	if info := p.Info(); info.Width != 4 || info.Height != 2 || info.FPS != 5 || info.Bitrate != 400_000 {
		t.Fatalf("Info = %+v", info)
	}
}

func TestSubstreamPipelineTapDropsMismatchedDimsAndNeverBlocks(t *testing.T) {
	enc := &recordingEncoder{done: make(chan struct{}, 32)}
	p := newTestPipeline(t, SubstreamOptions{
		SrcW: 8, SrcH: 4, Width: 4, Height: 2, Bitrate: 400_000, MainFPS: 10,
	}, enc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Run(ctx)
	defer p.Stop()

	frame := frame8x4()
	// Mismatched dims are dropped, not fed to the encoder.
	p.Tap(frame, 4, 4)
	p.Tap(frame, 8, 2)
	// A burst far beyond tap capacity must not block the tap caller.
	for i := 0; i < 100; i++ {
		p.Tap(frame, 8, 4)
	}
	if dropped := p.DroppedFrames(); dropped < 90 {
		t.Fatalf("expected the overflowing burst to be counted as dropped, got %d", dropped)
	}
}

func TestSubstreamPipelineFullRateWhenFPSUnset(t *testing.T) {
	enc := &recordingEncoder{done: make(chan struct{}, 32)}
	p := newTestPipeline(t, SubstreamOptions{
		SrcW: 8, SrcH: 4, Width: 4, Height: 2, Bitrate: 400_000, MainFPS: 15,
	}, enc)
	if info := p.Info(); info.FPS != 15 {
		t.Fatalf("fps=0 must follow the main rate, got %d", info.FPS)
	}
}

func TestSubstreamPipelineNoEncoderFailsLoud(t *testing.T) {
	_, err := NewSubstreamPipeline(SubstreamOptions{
		SrcW: 8, SrcH: 4, Width: 4, Height: 2, Bitrate: 1,
		EncoderDevice: "/dev/null", FFmpegBin: "",
		ProbeEncoder: func(string) (v4l2.ProbeResult, error) {
			return v4l2.ProbeResult{}, context.Canceled
		},
	})
	if err == nil {
		t.Fatal("expected an error when no encoder resolves and ffmpeg_bin is empty")
	}
}

func TestSubstreamPipelineFramesChannelClosesOnStop(t *testing.T) {
	enc := &recordingEncoder{done: make(chan struct{}, 32)}
	p := newTestPipeline(t, SubstreamOptions{
		SrcW: 8, SrcH: 4, Width: 4, Height: 2, Bitrate: 1, MainFPS: 10,
	}, enc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Run(ctx)

	closed := make(chan struct{})
	go func() {
		for range p.Frames() {
		}
		close(closed)
	}()
	p.Stop()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Frames channel must close when the pipeline stops")
	}
}

var _ = h264.NALU{} // keep the h264 import tied to the pipeline's contract

// ── runtime failover (field bug 2026-09-27: second M2M instance starves) ──

// failingEncoder fails every Encode and counts the attempts.
type failingEncoder struct {
	calls atomic.Int64
}

func (f *failingEncoder) Encode([]byte, uint64) error { f.calls.Add(1); return errBoom }
func (f *failingEncoder) Close() error                { return nil }
func (f *failingEncoder) Name() string                { return "failing" }

var errBoom = errors.New("boom")

// flakyEncoder fails the first n Encodes, then succeeds (records frames).
type flakyEncoder struct {
	recordingEncoder
	failuresLeft atomic.Int64
}

func (f *flakyEncoder) Encode(yuv []byte, pts uint64) error {
	if f.failuresLeft.Add(-1) >= 0 {
		return errBoom
	}
	return f.recordingEncoder.Encode(yuv, pts)
}

func tapN(p *SubstreamPipeline, frame []byte, n int) {
	for i := 0; i < n; i++ {
		p.Tap(frame, 8, 4)
	}
}

// tapPaced feeds taps slowly enough for the pipeline goroutine to consume
// each one — a full-speed burst only ever lands ~tapCapacity frames.
func tapPaced(p *SubstreamPipeline, frame []byte, n int) {
	for i := 0; i < n; i++ {
		p.Tap(frame, 8, 4)
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSubstreamFailoverToFallbackOnConsecutiveErrors(t *testing.T) {
	m2m := &failingEncoder{}
	fallback := &recordingEncoder{done: make(chan struct{}, 64)}
	p := newTestPipeline(t, SubstreamOptions{
		SrcW: 8, SrcH: 4, Width: 4, Height: 2, Bitrate: 400_000, MainFPS: 15,
		OpenFallback: func() (frameEncoder, error) { return fallback, nil },
	}, m2m)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Run(ctx)
	defer p.Stop()

	tapPaced(p, frame8x4(), 40)

	deadline := time.After(3 * time.Second)
	for fallback.count() < 10 {
		select {
		case <-fallback.done:
		case <-deadline:
			t.Fatalf("fallback never received frames (got %d)", fallback.count())
		}
	}
	// The M2M encoder must have been abandoned at exactly the budget.
	if got := m2m.calls.Load(); got != failoverBudget {
		t.Fatalf("M2M attempts = %d, want %d (failover must stop feeding the dead encoder)", got, failoverBudget)
	}
}

func TestSubstreamTransientErrorsDoNotTripFailover(t *testing.T) {
	flaky := &flakyEncoder{}
	flaky.failuresLeft.Store(3)
	flaky.done = make(chan struct{}, 64)
	fallback := &recordingEncoder{done: make(chan struct{}, 8)}
	p := newTestPipeline(t, SubstreamOptions{
		SrcW: 8, SrcH: 4, Width: 4, Height: 2, Bitrate: 400_000, MainFPS: 15,
		OpenFallback: func() (frameEncoder, error) { return fallback, nil },
	}, flaky)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Run(ctx)
	defer p.Stop()

	tapPaced(p, frame8x4(), 10)
	deadline := time.After(3 * time.Second)
	for flaky.count() < 7 {
		select {
		case <-flaky.done:
		case <-deadline:
			t.Fatalf("primary encoder never recovered (encoded %d)", flaky.count())
		}
	}
	if fallback.count() != 0 {
		t.Fatalf("failover must not trip on transient errors, but fallback encoded %d frames", fallback.count())
	}
}

func TestSubstreamDisablesAfterBothEncodersFail(t *testing.T) {
	m2m := &failingEncoder{}
	fallback := &failingEncoder{}
	p := newTestPipeline(t, SubstreamOptions{
		SrcW: 8, SrcH: 4, Width: 4, Height: 2, Bitrate: 400_000, MainFPS: 15,
		OpenFallback: func() (frameEncoder, error) { return fallback, nil },
	}, m2m)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Run(ctx)
	defer p.Stop()

	tapPaced(p, frame8x4(), 100)
	// Give the pipeline time to chew through the taps, then assert the
	// encode attempts are bounded: budget on each encoder, nothing after.
	time.Sleep(200 * time.Millisecond)
	beforeM2M, beforeFB := m2m.calls.Load(), fallback.calls.Load()
	if beforeM2M != failoverBudget || beforeFB != failoverBudget {
		t.Fatalf("attempts m2m=%d fallback=%d, want %d each", beforeM2M, beforeFB, failoverBudget)
	}
	time.Sleep(100 * time.Millisecond)
	if m2m.calls.Load() != beforeM2M || fallback.calls.Load() != beforeFB {
		t.Fatal("disabled pipeline must stop encoding entirely")
	}
	// Tapping a disabled pipeline must not block or panic.
	tapN(p, frame8x4(), 50)
}

// warnCountingHandler counts WARN records passing through slog.
type warnCountingHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *warnCountingHandler) Enabled(ctx context.Context, l slog.Level) bool { return true }
func (h *warnCountingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		h.mu.Lock()
		h.msgs = append(h.msgs, r.Message)
		h.mu.Unlock()
	}
	return nil
}
func (h *warnCountingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *warnCountingHandler) WithGroup(string) slog.Handler      { return h }
func (h *warnCountingHandler) warnCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.msgs)
}
func (h *warnCountingHandler) warnMessages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.msgs...)
}

func TestSubstreamFailureWarnsAreRateLimited(t *testing.T) {
	h := &warnCountingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prev)

	m2m := &failingEncoder{}
	fallback := &failingEncoder{}
	p := newTestPipeline(t, SubstreamOptions{
		SrcW: 8, SrcH: 4, Width: 4, Height: 2, Bitrate: 400_000, MainFPS: 15,
		OpenFallback: func() (frameEncoder, error) { return fallback, nil },
	}, m2m)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Run(ctx)
	defer p.Stop()

	tapPaced(p, frame8x4(), 100)
	time.Sleep(300 * time.Millisecond)
	// Exactly four WARNs: first M2M failure, failover notice, first
	// fallback failure, disable notice. The other 16 encode errors and
	// 80 drained taps must stay silent.
	if got := h.warnCount(); got != 4 {
		t.Fatalf("WARN records = %d, want 4 (rate-limited); got %q", got, h.warnMessages())
	}
}

// TestSubstreamPipelineFractionalDecimation: 15→10fps keeps 2 of every 3
// frames (integer division alone silently kept all 15 — the fps:10 config
// on the deployed .161 did nothing until this).
func TestSubstreamPipelineFractionalDecimation(t *testing.T) {
	enc := &recordingEncoder{done: make(chan struct{}, 32)}
	p := newTestPipeline(t, SubstreamOptions{
		SrcW: 8, SrcH: 4, Width: 4, Height: 2,
		FPs: 10, Bitrate: 400_000, MainFPS: 15,
	}, enc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Run(ctx)
	defer p.Stop()

	frame := frame8x4()
	for tag := byte(1); tag <= 6; tag++ {
		for i := range frame {
			if i < 32 {
				frame[i] = tag
			}
		}
		p.Tap(frame, 8, 4)
		time.Sleep(20 * time.Millisecond)
	}

	deadline := time.After(3 * time.Second)
	for enc.count() < 4 {
		select {
		case <-enc.done:
		case <-deadline:
			t.Fatalf("timed out waiting for encodes, got %d (want 4 of 6)", enc.count())
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := enc.count(); got != 4 {
		t.Fatalf("15→10fps must encode 4 of 6 frames, got %d", got)
	}
	// Pattern check: with period=3 keep=2 the dropped tags are 2 and 5.
	for i, want := range []byte{1, 3, 4, 6} {
		if enc.frame(i)[0] != want {
			t.Fatalf("encoded tags = %d,%d,%d,%d, want 1,3,4,6",
				enc.frame(0)[0], enc.frame(1)[0], enc.frame(2)[0], enc.frame(3)[0])
		}
	}
	if info := p.Info(); info.FPS != 10 {
		t.Fatalf("Info fps = %d, want 10", info.FPS)
	}
}
