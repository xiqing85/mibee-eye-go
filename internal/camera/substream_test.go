package camera

import (
	"context"
	"sync"
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

func newTestPipeline(t *testing.T, o SubstreamOptions, enc *recordingEncoder) *SubstreamPipeline {
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
