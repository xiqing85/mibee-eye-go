// camera.mode: v4l2 — generic V4L2 backend for non-Pi boards.
//
// Capture is pure-Go V4L2 MMAP (YU12/I420, any /dev/videoN incl. USB/UVC).
// Encoding resolves at Start: when camera.encoder_device probes as an M2M
// H.264 node (bcm2835-codec / i.MX coda …) frames are hardware-encoded in
// pure Go; otherwise a resident ffmpeg subprocess encodes rawvideo→H.264
// (stdin/stdout pipes, restart-on-death, matching this repo's subprocess
// culture from mtxrpicam and the AI decoder).
//
// Frames carry H.264 Annex-B access units, identical to the other backends.
package camera

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/xiqing85/mibee-eye-go/internal/h264"
	"github.com/xiqing85/mibee-eye-go/internal/v4l2"
)

// frameEncoder is the encoding half of the v4l2 backend.
type frameEncoder interface {
	// Encode submits one I420 frame. M2M returns the encoded AU inline;
	// ffmpeg pushes AUs asynchronously through its own reader goroutine
	// (in which case Encode returns nil, nil).
	Encode(yuv []byte, pts uint64) error
	Close() error
	Name() string
}

// V4L2Source implements Camera on top of the v4l2 package.
type V4L2Source struct {
	device        string
	encoderDevice string
	params        Params
	info          CameraInfo
	ffmpegBin     string

	framesCh  chan Frame
	paramsMu  sync.Mutex
	memParams map[string]interface{}
	dropped   uint64

	capture captureDevice
	encoder frameEncoder
	stopCh  chan struct{}
	wg      sync.WaitGroup

	// Injection points so tests can cover selection without hardware.
	probeEncoder func(path string) (v4l2.ProbeResult, error)
	openCapture  func(path string, w, h uint32) (captureDevice, error)
	openM2M      func(path string, w, h uint32) (frameEncoder, error)
}

// captureDevice is the subset of *v4l2.Capture the source needs (mockable).
type captureDevice interface {
	ReadFrame() ([]byte, error)
	Close()
}

// V4L2 functional options, mirroring the other backends.
type V4L2Option func(*V4L2Source)

// WithV4L2Device sets the capture node (required; default lives in config).
func WithV4L2Device(path string) V4L2Option {
	return func(s *V4L2Source) { s.device = path }
}

// WithV4L2EncoderDevice sets the M2M encoder node to probe (required;
// default lives in config).
func WithV4L2EncoderDevice(path string) V4L2Option {
	return func(s *V4L2Source) { s.encoderDevice = path }
}

// WithV4L2Params carries width/height/fps/bitrate and imaging defaults.
func WithV4L2Params(p Params) V4L2Option {
	return func(s *V4L2Source) { s.params = p }
}

// WithV4L2Info sets the CameraInfo reported to ONVIF.
func WithV4L2Info(i CameraInfo) V4L2Option {
	return func(s *V4L2Source) { s.info = i }
}

// WithV4L2FFmpegBin sets the ffmpeg binary for the fallback encoder
// (required when the ffmpeg path is taken; default lives in config).
func WithV4L2FFmpegBin(bin string) V4L2Option {
	return func(s *V4L2Source) { s.ffmpegBin = bin }
}

// WithV4L2Probe overrides the encoder-node probe (tests).
func WithV4L2Probe(f func(path string) (v4l2.ProbeResult, error)) V4L2Option {
	return func(s *V4L2Source) { s.probeEncoder = f }
}

// WithV4L2Capture overrides capture construction (tests).
func WithV4L2Capture(f func(path string, w, h uint32) (captureDevice, error)) V4L2Option {
	return func(s *V4L2Source) { s.openCapture = f }
}

// WithV4L2M2M overrides M2M encoder construction (tests).
func WithV4L2M2M(f func(path string, w, h uint32) (frameEncoder, error)) V4L2Option {
	return func(s *V4L2Source) { s.openM2M = f }
}

// NewV4L2Source builds the backend from options.
func NewV4L2Source(opts ...V4L2Option) *V4L2Source {
	s := &V4L2Source{
		// No invented defaults: main wires every value from config
		// (camera.device / camera.encoder_device / camera.ffmpeg_bin).
		framesCh:     make(chan Frame, 30),
		memParams:    map[string]interface{}{},
		stopCh:       make(chan struct{}),
		probeEncoder: v4l2.ProbeEncoder,
		openCapture: func(path string, w, h uint32) (captureDevice, error) {
			return v4l2.OpenCapture(path, w, h)
		},
		openM2M: func(path string, w, h uint32) (frameEncoder, error) {
			enc, err := v4l2.OpenM2MEncoder(path, w, h)
			if err != nil {
				return nil, err
			}
			return &m2mEncoder{enc: enc}, nil
		},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Frames implements Camera.
func (s *V4L2Source) Frames() <-chan Frame { return s.framesCh }

// DroppedFrames reports frames dropped by a slow consumer (parity with
// the subprocess backends).
func (s *V4L2Source) DroppedFrames() uint64 { return s.dropped }

// Info implements Camera.
func (s *V4L2Source) Info() CameraInfo { return s.info }

// SetParam stores imaging parameters in memory. Values cannot reach the
// sensor through this backend (same contract as the rtsp source): the web
// UI stays consistent and ONVIF reads them back.
func (s *V4L2Source) SetParam(name string, value interface{}) error {
	s.paramsMu.Lock()
	defer s.paramsMu.Unlock()
	s.memParams[name] = value
	return nil
}

// GetParam implements Camera.
func (s *V4L2Source) GetParam(name string) (interface{}, error) {
	s.paramsMu.Lock()
	defer s.paramsMu.Unlock()
	if v, ok := s.memParams[name]; ok {
		return v, nil
	}
	return getParamValue(s.params, name)
}

// Start implements Camera: opens capture, resolves the encoder and spawns
// the capture pump.
func (s *V4L2Source) Start(ctx context.Context) error {
	w := uint32(s.params.Width)
	h := uint32(s.params.Height)

	if s.device == "" {
		return fmt.Errorf("v4l2 source: capture device not configured (camera.device)")
	}
	cap, err := s.openCapture(s.device, w, h)
	if err != nil {
		return fmt.Errorf("v4l2 source: capture: %w", err)
	}
	s.capture = cap

	emit := s.emit
	if res, err := s.probeEncoder(s.encoderDevice); err == nil && res.M2MCapable {
		if enc, err := s.openM2M(s.encoderDevice, w, h); err == nil {
			if m, ok := enc.(*m2mEncoder); ok {
				m.onAU = emit
			}
			s.encoder = enc
		} else {
			slog.Warn("v4l2 source: M2M encoder open failed, falling back to ffmpeg", "error", err)
		}
	} else if err != nil {
		slog.Info("v4l2 source: no M2M encoder, using ffmpeg fallback", "device", s.encoderDevice, "error", err)
	}
	if s.encoder == nil {
		if s.ffmpegBin == "" { // test escape: encoder chosen explicitly as nil-ffmpeg
			return fmt.Errorf("v4l2 source: no encoder available")
		}
		fe, err := newFFmpegEncoder(s.ffmpegBin, s.params, func(au []h264.NALU, key bool) {
			s.emit(au, key)
		})
		if err != nil {
			s.capture.Close()
			s.capture = nil
			return fmt.Errorf("v4l2 source: %w", err)
		}
		s.encoder = fe
	}
	slog.Info("v4l2 source: capture started", "device", s.device, "encoder", s.encoder.Name(),
		"width", w, "height", h)

	s.wg.Add(1)
	go s.pump()
	return nil
}

// Stop implements Camera.
func (s *V4L2Source) Stop() error {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	s.wg.Wait()
	if s.encoder != nil {
		_ = s.encoder.Close()
		s.encoder = nil
	}
	if s.capture != nil {
		s.capture.Close()
		s.capture = nil
	}
	return nil
}

func (s *V4L2Source) pump() {
	defer s.wg.Done()
	start := time.Now()
	var count uint64
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}
		frame, err := s.capture.ReadFrame()
		if err != nil {
			select {
			case <-s.stopCh:
				return
			case <-time.After(time.Second):
			}
			continue
		}
		yuv := make([]byte, len(frame))
		copy(yuv, frame) // leave the MMAP buffer before the next DQBUF
		count++
		pts := uint64(time.Since(start).Milliseconds()) * 90
		if err := s.encoder.Encode(yuv, pts); err != nil {
			slog.Warn("v4l2 source: encode failed", "error", err)
		}
	}
}

func (s *V4L2Source) emit(nalus []h264.NALU, key bool) {
	if len(nalus) == 0 {
		return
	}
	data := make([]byte, 0, 4096)
	for _, n := range nalus {
		data = append(data, 0, 0, 0, 1)
		data = append(data, n.Data...)
	}
	f := Frame{
		Data:      data,
		Timestamp: time.Now(),
		PTS:       0,
	}
	select {
	case s.framesCh <- f:
	default:
		s.dropped++
	}
}

// ── M2M wrapper ──────────────────────────────────────────────────────────

type m2mEncoder struct {
	enc  *v4l2.M2MEncoder
	onAU func(nalus []h264.NALU, key bool)
}

func (m *m2mEncoder) Name() string { return "v4l2-m2m" }

func (m *m2mEncoder) Encode(yuv []byte, pts uint64) error {
	au, err := m.enc.Encode(yuv)
	if err != nil {
		return err
	}
	nalus := h264.NewParser().Parse(au)
	key := false
	for _, n := range nalus {
		if n.IsIDR {
			key = true
			break
		}
	}
	m.onAU(nalus, key)
	return nil
}

func (m *m2mEncoder) Close() error {
	m.enc.Close()
	return nil
}

// ── ffmpeg wrapper ───────────────────────────────────────────────────────

type ffmpegEncoder struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	parser *h264.Parser
	wg     sync.WaitGroup
}

func newFFmpegEncoder(bin string, p Params, onAU func(nalus []h264.NALU, key bool)) (*ffmpegEncoder, error) {
	gop := uint32(30)
	if p.FPS > 1 {
		gop = uint32(p.FPS) * 2 // IDR every ~2s, matching the Rust twin
	}
	cmd := exec.Command(bin,
		"-hide_banner", "-loglevel", "error",
		"-f", "rawvideo", "-pix_fmt", "yuv420p",
		"-s", fmt.Sprintf("%dx%d", uint32(p.Width), uint32(p.Height)),
		"-r", fmt.Sprintf("%g", p.FPS),
		"-i", "pipe:0",
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency",
		"-profile:v", "baseline",
		"-pix_fmt", "yuv420p",
		"-b:v", fmt.Sprintf("%d", uint32(p.Bitrate)),
		"-g", fmt.Sprintf("%d", gop),
		"-f", "h264", "pipe:1",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg start: %w", err)
	}
	e := &ffmpegEncoder{cmd: cmd, stdin: stdin, stdout: stdout, parser: h264.NewParser()}
	e.wg.Add(1)
	go e.readLoop(onAU)
	return e, nil
}

func (e *ffmpegEncoder) Name() string { return "ffmpeg-libx264" }

func (e *ffmpegEncoder) Encode(yuv []byte, pts uint64) error {
	_, err := e.stdin.Write(yuv)
	return err
}

func (e *ffmpegEncoder) Close() error {
	_ = e.stdin.Close()
	_ = e.cmd.Process.Kill()
	e.wg.Wait()
	return nil
}

// readLoop splits the ffmpeg Annex-B byte stream into access units using
// the same NALU/AU helpers as the rpicam-vid backend.
func (e *ffmpegEncoder) readLoop(onAU func([]h264.NALU, bool)) {
	defer e.wg.Done()
	buf := make([]byte, 32*1024)
	var pending []byte
	var cur []h264.NALU
	for {
		n, err := e.stdout.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			var nalus []h264.NALU
			nalus, pending = extractCompleteNALUs(pending, e.parser)
			for _, nalu := range nalus {
				if len(cur) > 0 && startsNewAU(nalu, cur) {
					e.flush(cur, onAU)
					cur = nil
				}
				cur = append(cur, nalu)
			}
		}
		if err != nil {
			if len(cur) > 0 {
				e.flush(cur, onAU)
			}
			return
		}
	}
}

func (e *ffmpegEncoder) flush(cur []h264.NALU, onAU func([]h264.NALU, bool)) {
	key := false
	for _, n := range cur {
		if n.IsSPS || n.IsPPS || (len(n.Data) > 0 && n.Data[0]&0x1f == 5) {
			key = true
		}
	}
	onAU(cur, key)
}
