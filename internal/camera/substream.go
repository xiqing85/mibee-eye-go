package camera

// substream.go — the bandwidth-saving substream pipeline (SPEC appendix
// A #20): a second encoder session fed by a bounded, drop-on-full tap of
// the main capture frames (already rotated/flipped), downscaled to the
// configured geometry.
//
//	   main capture → transform → main encoder ──► Frames()
//	                    │ Tap (copy, drop-on-full)
//	                    ▼
//	              SubstreamPipeline.Run ──► second encoder session
//	                   (downscale + fps        (V4L2 M2M or ffmpeg)
//	                    decimation)
//
// The pipeline is boot-static (created in main from the boot config; the
// camera_restart in-place rebuild keeps tapping the same instance — the
// tap validates the per-frame effective dims against the boot geometry).

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xiqing85/mibee-eye-go/internal/h264"
	"github.com/xiqing85/mibee-eye-go/internal/v4l2"
)

// tapCapacity frames of headroom for encoder jitter; a full tap drops
// (the sub stream resynchronises on its next IDR).
const tapCapacity = 2

// SubstreamOptions carries the [camera.substream] config plus the encoder
// plumbing shared with the main pipeline.
type SubstreamOptions struct {
	SrcW, SrcH    int // main effective (post-rotation) frame dims
	Width, Height int // sub geometry (even, <= SrcW/SrcH)
	FPs           int // 0 = follow the main fps
	Bitrate       int

	EncoderDevice string // V4L2 M2M node
	FFmpegBin     string // ffmpeg fallback
	MainFPS       int    // main fps (drives the decimation ratio)

	// Injectable for tests (mirrors the camera backends).
	ProbeEncoder func(path string) (v4l2.ProbeResult, error)
	OpenM2M      func(path string, w, h uint32) (frameEncoder, error)
}

// SubstreamPipeline owns the second encoder session and the sub frame
// channel.
type SubstreamPipeline struct {
	opts     SubstreamOptions
	subFPS   int
	fpsDiv   int // emit every fpsDiv-th tapped frame (>=1)
	enc      frameEncoder
	framesCh chan Frame
	tapCh    chan []byte
	stopCh   chan struct{}
	wg       sync.WaitGroup

	counter atomic.Uint64
	dropped atomic.Uint64
}

// SubstreamInfo is the geometry handshake to consumers (ONVIF profile,
// web init segment).
type SubstreamInfo struct {
	Width, Height, FPS, Bitrate int
}

// NewSubstreamPipeline resolves the encoder (M2M probe → ffmpeg fallback,
// same order as the camera backends) and prepares the pipeline. Call Run
// once the main camera has started.
func NewSubstreamPipeline(o SubstreamOptions) (*SubstreamPipeline, error) {
	subFPS := o.FPs
	if subFPS <= 0 {
		subFPS = o.MainFPS
	}
	if subFPS <= 0 {
		subFPS = 15
	}
	fpsDiv := 1
	if o.MainFPS > subFPS {
		fpsDiv = o.MainFPS / subFPS
	}

	p := &SubstreamPipeline{
		opts:     o,
		subFPS:   subFPS,
		fpsDiv:   fpsDiv,
		framesCh: make(chan Frame, 16),
		tapCh:    make(chan []byte, tapCapacity),
		stopCh:   make(chan struct{}),
	}

	probe := o.ProbeEncoder
	if probe == nil {
		probe = v4l2.ProbeEncoder
	}
	openM2M := o.OpenM2M
	if openM2M == nil {
		openM2M = func(path string, w, h uint32) (frameEncoder, error) {
			enc, err := v4l2.OpenM2MEncoder(path, w, h, v4l2.M2MEncoderOptions{
				Bitrate: int32(o.Bitrate),
				IPeriod: int32(subFPS * 2), // IDR every ~2s of sub-rate video
			})
			if err != nil {
				return nil, err
			}
			return &m2mEncoder{enc: enc}, nil
		}
	}

	emit := func(nalus []h264.NALU, key bool) { p.emit(nalus, key) }
	if res, err := probe(o.EncoderDevice); err == nil && res.M2MCapable {
		if enc, err := openM2M(o.EncoderDevice, uint32(o.Width), uint32(o.Height)); err == nil {
			if m, ok := enc.(*m2mEncoder); ok {
				m.onAU = emit
			}
			p.enc = enc
		} else {
			slog.Warn("substream: M2M open failed, falling back to ffmpeg", "error", err)
		}
	} else if err != nil {
		slog.Info("substream: no M2M encoder, using ffmpeg fallback", "device", o.EncoderDevice, "error", err)
	}
	if p.enc == nil {
		if o.FFmpegBin == "" {
			return nil, fmt.Errorf("substream: no encoder available (no M2M at %q and camera.ffmpeg_bin is empty)", o.EncoderDevice)
		}
		fp := DefaultParams()
		fp.Width, fp.Height = uint32(o.Width), uint32(o.Height)
		fp.FPS = float32(subFPS)
		fp.Bitrate = uint32(o.Bitrate)
		fe, err := newFFmpegEncoder(o.FFmpegBin, fp, emit)
		if err != nil {
			return nil, fmt.Errorf("substream: ffmpeg fallback encoder: %w", err)
		}
		p.enc = fe
	}
	return p, nil
}

// Info reports the sub geometry (ONVIF sub profile, web init segment).
func (p *SubstreamPipeline) Info() SubstreamInfo {
	return SubstreamInfo{Width: p.opts.Width, Height: p.opts.Height, FPS: p.subFPS, Bitrate: p.opts.Bitrate}
}

// Frames returns the sub H.264 access-unit channel (closed when the
// pipeline stops).
func (p *SubstreamPipeline) Frames() <-chan Frame { return p.framesCh }

// DroppedFrames reports frames dropped by the tap or a slow consumer.
func (p *SubstreamPipeline) DroppedFrames() uint64 { return p.dropped.Load() }

// Tap offers a post-transform main frame (w×h I420) to the pipeline.
// Non-blocking and copy-on-tap: a slow sub pipeline drops frames and
// never stalls the main capture/encode path. The frame buffer belongs to
// the caller (reused frame to frame), hence the copy. Frames whose dims
// do not match the boot geometry are dropped — a geometry-crossing
// restart goes through the full process restart, not the in-place path.
func (p *SubstreamPipeline) Tap(frame []byte, w, h uint32) {
	if w != uint32(p.opts.SrcW) || h != uint32(p.opts.SrcH) {
		p.dropped.Add(1)
		return
	}
	buf := make([]byte, len(frame))
	copy(buf, frame)
	select {
	case p.tapCh <- buf:
	default:
		p.dropped.Add(1)
	}
}

// Run consumes tapped frames until ctx is canceled or Stop is called:
// decimate to the sub rate, downscale, encode. Spawns one goroutine.
func (p *SubstreamPipeline) Run(ctx context.Context) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(p.framesCh)
		defer func() {
			if p.enc != nil {
				_ = p.enc.Close()
			}
		}()
		start := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.stopCh:
				return
			case frame, ok := <-p.tapCh:
				if !ok {
					return
				}
				p.counter.Add(1)
				if p.fpsDiv > 1 && p.counter.Load()%uint64(p.fpsDiv) != 0 {
					continue
				}
				sub := DownscaleYU12(frame, uint32(p.opts.SrcW), uint32(p.opts.SrcH),
					uint32(p.opts.Width), uint32(p.opts.Height))
				pts := uint64(time.Since(start).Milliseconds()) * 90
				if err := p.enc.Encode(sub, pts); err != nil {
					slog.Warn("substream: encode failed", "error", err)
				}
			}
		}
	}()
}

// Stop tears the pipeline down (idempotent; also triggered by ctx).
func (p *SubstreamPipeline) Stop() {
	select {
	case <-p.stopCh:
	default:
		close(p.stopCh)
	}
	p.wg.Wait()
}

// emit builds an Annex-B access unit from the encoder callback and
// delivers it to the sub frames channel (drop-on-full).
func (p *SubstreamPipeline) emit(nalus []h264.NALU, key bool) {
	if len(nalus) == 0 {
		return
	}
	data := make([]byte, 0, 4096)
	for _, n := range nalus {
		data = append(data, 0, 0, 0, 1)
		data = append(data, n.Data...)
	}
	f := Frame{Data: data, Timestamp: time.Now()}
	select {
	case p.framesCh <- f:
	default:
		p.dropped.Add(1)
	}
}
