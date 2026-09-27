package camera

// substream.go — the bandwidth-saving substream pipeline (SPEC appendix
// A #20): a second encoder session fed by a bounded, drop-on-full tap of
// the main capture frames (already rotated/flipped), downscaled to the
// configured geometry.
//
//	   main capture → transform → main encoder ──► Frames()
//	                    │ Tap (downscale in-place on the caller's live
//	                    ▼        frame, drop-on-full, pooled output)
//	              SubstreamPipeline.Run ──► second encoder session
//	                   (fps decimation          (V4L2 M2M or ffmpeg)
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

// gcd returns the greatest common divisor (decimation math).
func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// tapCapacity frames of headroom for encoder jitter; a full tap drops
// (the sub stream resynchronises on its next IDR).
const tapCapacity = 2

// failoverBudget consecutive Encode errors switch the pipeline to the
// ffmpeg fallback; the same budget on the fallback disables the pipeline
// (SPEC allows the sub endpoints to 404). Field-proven need: a second M2M
// encoder instance on bcm2835 can starve permanently behind the main
// encoder (.161, 2026-09-27) — the sync v4l2 Encode leaves OUTPUT buffers
// queued on poll timeout, so every later QBUF fails with EINVAL and the
// errors never self-heal.
const failoverBudget = 10

// warnEveryN rate-limits repeated encode-failure WARNs — the disabling
// path fires once per frame otherwise and flooded the journal at ~12/s.
const warnEveryN = 100

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
	// OpenFallback builds the runtime failover encoder (default: the
	// ffmpeg subprocess). Called on the Run goroutine at failover time.
	OpenFallback func() (frameEncoder, error)
}

// SubstreamPipeline owns the second encoder session and the sub frame
// channel.
type SubstreamPipeline struct {
	opts        SubstreamOptions
	subFPS      int
	decimPeriod uint32 // fractional decimation: of every `period` main
	decimKeep   uint32 // frames, `keep` reach the encoder (both >= 1)
	enc         frameEncoder
	framesCh    chan Frame
	tapCh       chan []byte
	stopCh      chan struct{}
	wg          sync.WaitGroup

	// Failover state — `disabled` is atomic: Tap (main pipeline
	// goroutine) reads it to stop downscaling work the moment the
	// pipeline gives up; the rest is owned by the Run goroutine.
	openFallback func() (frameEncoder, error)
	onFallback   bool
	disabled     atomic.Bool

	// subPool recycles the downscaled sub frames between Tap and Run —
	// the only remaining per-frame allocation on the sub path.
	subPool sync.Pool

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
	// Fractional decimation: 15→10fps keeps 2 of every 3 frames (integer
	// division alone would silently keep all 15).
	period, keep := 1, 1
	if o.MainFPS > subFPS {
		g := gcd(o.MainFPS, subFPS)
		period, keep = o.MainFPS/g, subFPS/g
	}

	p := &SubstreamPipeline{
		opts:        o,
		subFPS:      subFPS,
		decimPeriod: uint32(period),
		decimKeep:   uint32(keep),
		framesCh:    make(chan Frame, 16),
		tapCh:       make(chan []byte, tapCapacity),
		stopCh:      make(chan struct{}),
	}
	p.subPool.New = func() any {
		ch := int(max(uint32(o.Height)/2, 1))
		cw := int(max(uint32(o.Width)/2, 1))
		b := make([]byte, o.Width*o.Height+2*ch*cw)
		return &b
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
	p.openFallback = o.OpenFallback
	if p.openFallback == nil {
		p.openFallback = func() (frameEncoder, error) {
			if o.FFmpegBin == "" {
				return nil, fmt.Errorf("substream: no fallback encoder (camera.ffmpeg_bin is empty)")
			}
			fp := DefaultParams()
			fp.Width, fp.Height = uint32(o.Width), uint32(o.Height)
			fp.FPS = float32(subFPS)
			fp.Bitrate = uint32(o.Bitrate)
			return newSubstreamFFmpegEncoder(o.FFmpegBin, fp, emit)
		}
	}
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
// Non-blocking: a slow sub pipeline drops frames and never stalls the
// main capture/encode path. The frame buffer belongs to the caller
// (reused frame to frame), so instead of copying the full main frame it
// is consumed synchronously, read-only, while it is still valid: Tap
// downscales into a pooled sub-sized buffer and only that small frame is
// queued — the 0.9MB-per-kept-frame tap copy is gone. Frames whose dims
// do not match the boot geometry are dropped — a geometry-crossing
// restart goes through the full process restart, not the in-place path.
// Decimation is decided before any work: a dropped-by-cadence frame
// costs nothing.
func (p *SubstreamPipeline) Tap(frame []byte, w, h uint32) {
	if w != uint32(p.opts.SrcW) || h != uint32(p.opts.SrcH) {
		p.dropped.Add(1)
		return
	}
	if p.disabled.Load() {
		p.dropped.Add(1)
		return
	}
	if p.decimPeriod > 1 {
		n := p.counter.Add(1)
		if uint32(n)%p.decimPeriod >= p.decimKeep {
			p.dropped.Add(1)
			return
		}
	}
	bufp := p.subPool.Get().(*[]byte)
	sub := DownscaleYU12Into((*bufp)[:0], frame, uint32(p.opts.SrcW), uint32(p.opts.SrcH),
		uint32(p.opts.Width), uint32(p.opts.Height))
	select {
	case p.tapCh <- sub:
	default:
		p.dropped.Add(1)
		p.put(sub)
	}
}

// put returns a sub frame's buffer to the pool (no-op for empty slices).
func (p *SubstreamPipeline) put(b []byte) {
	if cap(b) == 0 {
		return
	}
	full := b[:cap(b)]
	p.subPool.Put(&full)
}

// Run consumes the tapped sub frames until ctx is canceled or Stop is
// called: encode each (already downscaled, pooled) frame and recycle its
// buffer. Spawns one goroutine.
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
		var consec int // consecutive Encode errors (reset on success)
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
				if p.disabled.Load() {
					p.put(frame) // drain the tap, keep the main path unblocked
					continue
				}
				pts := uint64(time.Since(start).Milliseconds()) * 90
				err := p.enc.Encode(frame, pts)
				p.put(frame)
				if err != nil {
					consec++
					if consec == 1 || consec%warnEveryN == 0 {
						slog.Warn("substream: encode failed", "error", err, "consecutive", consec)
					}
					if consec >= failoverBudget {
						p.failover(&consec)
					}
					continue
				}
				consec = 0
			}
		}
	}()
}

// failover replaces a dead encoder (M2M → ffmpeg) or disables the
// pipeline when the fallback is dead too. Runs on the Run goroutine.
func (p *SubstreamPipeline) failover(consec *int) {
	name := "<nil>"
	if p.enc != nil {
		name = p.enc.Name()
		_ = p.enc.Close()
		p.enc = nil
	}
	if p.onFallback {
		p.disabled.Store(true)
		*consec = 0
		slog.Warn("substream: disabled — fallback encoder keeps failing",
			"tried", name, "note", "sub endpoints will 404 until restart")
		return
	}
	enc, err := p.openFallback()
	if err != nil {
		p.disabled.Store(true)
		*consec = 0
		slog.Warn("substream: disabled — encoder failed and no fallback available",
			"tried", name, "error", err)
		return
	}
	p.enc = enc
	p.onFallback = true
	*consec = 0
	slog.Warn("substream: encoder failed repeatedly, switching to fallback",
		"from", name, "to", enc.Name())
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
