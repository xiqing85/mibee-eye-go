package ai

// yuv_source.go — direct YUV tap frame source. When the camera pipeline
// already holds raw I420 frames in-process (rpicam-vid rotation 90/270
// raw-YUV path, or the v4l2 backend), inference consumes those frames
// directly instead of the encode → AUHub → ffmpeg keyframe-decode round
// trip: one subprocess, two pipes and the IDR-cadence coupling are gone,
// and the conversion keeps the frame's aspect ratio (the ffmpeg path
// scaled every geometry to a fixed 320×240).
//
// The camera backend calls Tap once per post-transform frame; Tap
// converts at most one frame per interval into a small aspect-preserving
// RGB24 image (even dims, large side ≤ yuvTapMaxSide — preprocess
// stretches to the model input from there, exactly as it does for ffmpeg
// frames). Non-blocking end to end: a busy inference loop drops frames,
// never stalls capture.

import (
	"context"
	"sync"
	"time"

	"github.com/xiqing85/mibee-eye-go/internal/camera"
)

// yuvTapMaxSide bounds the converted frame's larger dimension.
const yuvTapMaxSide = 320

// YUVSource is a push-based FrameSource over in-process camera frames.
type YUVSource struct {
	interval time.Duration
	frames   chan Frame

	mu     sync.Mutex // Tap is called from one capture goroutine; guards the buffers below
	last   time.Time
	yuvBuf []byte
	rgbBuf []byte

	ctx context.Context
}

// NewYUVSource builds the source. interval ≤ 0 converts every tapped
// frame (the service loop still rate-limits inference itself).
func NewYUVSource(interval time.Duration) *YUVSource {
	return &YUVSource{
		interval: interval,
		frames:   make(chan Frame, 1),
	}
}

// Frames returns the converted-frame channel (drop-on-full).
func (s *YUVSource) Frames() <-chan Frame { return s.frames }

// Start records the service context; cancelled contexts make Tap a no-op.
func (s *YUVSource) Start(ctx context.Context) { s.ctx = ctx }

// Describe identifies the source in the startup log.
func (s *YUVSource) Describe() string {
	return "in-process YUV tap (no ffmpeg decode)"
}

// Tap offers one post-transform I420 frame. Bounded work on the capture
// goroutine (small downscale + one conversion, at most once per
// interval); frame may be reused by the caller as soon as Tap returns.
func (s *YUVSource) Tap(frame []byte, w, h uint32) {
	if s.ctx != nil && s.ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.interval > 0 && time.Since(s.last) < s.interval {
		return
	}
	s.last = time.Now()

	dw, dh := fitEven(w, h, yuvTapMaxSide)
	yuv := camera.DownscaleYU12Into(s.yuvBuf, frame, w, h, dw, dh)
	s.yuvBuf = yuv[:cap(yuv)]
	need := int(dw) * int(dh) * 3
	if cap(s.rgbBuf) < need {
		s.rgbBuf = make([]byte, need)
	}
	rgb := s.rgbBuf[:need]
	yu12ToRGB24(rgb, yuv, int(dw), int(dh))

	out := make([]byte, need) // copy-out: rgbBuf is reused on the next tap
	copy(out, rgb)
	select {
	case s.frames <- Frame{Width: dw, Height: dh, Data: out}:
	default: // inference busy — drop this frame, keep the next
	}
}

// fitEven shrinks (w, h) into a max×max box preserving aspect, with even
// dimensions (I420 chroma) clamped to at least 2.
func fitEven(w, h, box uint32) (uint32, uint32) {
	if w <= box && h <= box {
		return evenDim(w), evenDim(h)
	}
	if w >= h {
		return evenDim(box), evenDim(h * box / w)
	}
	return evenDim(w * box / h), evenDim(box)
}

func evenDim(v uint32) uint32 {
	if v < 2 {
		return 2
	}
	return v &^ 1
}

// yu12ToRGB24 converts a contiguous I420 frame to interleaved RGB24
// (BT.601 limited-range YUV → full-range RGB, matching ffmpeg's default
// yuv420p→rgb24 swscale so the model sees comparable input either way).
func yu12ToRGB24(dst, src []byte, w, h int) {
	ySize := w * h
	cW := w / 2
	cH := h / 2
	uOff := ySize
	vOff := ySize + cW*cH
	di := 0
	for row := 0; row < h; row++ {
		yRow := src[row*w : row*w+w]
		cRow := (row / 2) * cW
		uRow := src[uOff+cRow : uOff+cRow+cW]
		vRow := src[vOff+cRow : vOff+cRow+cW]
		for col := 0; col < w; col++ {
			y := int(yRow[col]) - 16
			u := int(uRow[col/2]) - 128
			v := int(vRow[col/2]) - 128
			c := 298*y + 128
			dst[di] = clamp8((c + 409*v) >> 8)
			dst[di+1] = clamp8((c - 100*u - 208*v) >> 8)
			dst[di+2] = clamp8((c + 516*u) >> 8)
			di += 3
		}
	}
}

func clamp8(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}
