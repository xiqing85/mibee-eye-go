// rtsp_source.go implements an RTSP client camera backend.
// Instead of spawning mtxrpicam for direct camera capture, it connects to
// an external RTSP server (e.g. MediaMTX) and consumes the H.264 stream.
// This is used when the bundled mtxrpicam binary is incompatible with the
// device's hardware encoder (common after camera swap or OS upgrade).

package camera

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	rtph264 "github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/rtp"
)

// RTSPSource implements Camera by consuming an RTSP stream.
// It connects to an external RTSP server (typically MediaMTX running on the
// same device) and depacketizes H.264 RTP packets into Frame structs.
type RTSPSource struct {
	mu       sync.RWMutex
	url      string
	info     CameraInfo
	params   Params
	framesCh chan Frame
	stopOnce sync.Once
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewRTSPSource creates a new RTSP-consuming camera backend.
func NewRTSPSource(url string, params Params, info CameraInfo) *RTSPSource {
	return &RTSPSource{
		url:      url,
		info:     info,
		params:   params,
		framesCh: make(chan Frame, 30),
	}
}

func (s *RTSPSource) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	ctx, s.cancel = context.WithCancel(ctx)
	s.wg.Add(1)
	go s.run(ctx)
	return nil
}

func (s *RTSPSource) Stop() error {
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		s.wg.Wait()

		s.mu.Lock()
		if s.framesCh != nil {
			close(s.framesCh)
			s.framesCh = nil
		}
		s.mu.Unlock()
	})
	return nil
}

func (s *RTSPSource) Frames() <-chan Frame {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.framesCh
}

// SetParam stores parameter changes in memory. Since the actual camera is
// controlled by MediaMTX (or another RTSP source), parameter changes cannot
// be applied to the hardware directly. They are stored for read-back via
// GetParam and reflected in the ONVIF/web UI.
func (s *RTSPSource) SetParam(name string, value interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	paramName, ok := mapParamName(name)
	if !ok {
		return fmt.Errorf("unknown parameter: %s", name)
	}
	if err := setParamValue(&s.params, paramName, value); err != nil {
		return fmt.Errorf("set %s: %w", name, err)
	}
	s.info.Width = s.params.Width
	s.info.Height = s.params.Height
	s.info.FPS = s.params.FPS
	return nil
}

func (s *RTSPSource) GetParam(name string) (interface{}, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	paramName, ok := mapParamName(name)
	if !ok {
		return nil, fmt.Errorf("unknown parameter: %s", name)
	}
	return getParamValue(s.params, paramName)
}

func (s *RTSPSource) Info() CameraInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.info
}

// run connects to the RTSP source and maintains the connection with
// automatic reconnection on failure using exponential backoff.
func (s *RTSPSource) run(ctx context.Context) {
	defer s.wg.Done()

	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		framesReceived, err := s.connect(ctx)
		if ctx.Err() != nil {
			return
		}
		// Reset backoff if we received at least one frame during the session.
		// This prevents exponential backoff from accumulating across healthy
		// but short-lived reconnections.
		if framesReceived {
			backoff = time.Second
		}

		slog.Warn("camera: rtsp source disconnected, reconnecting", "error", err, "backoff", backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff = min(backoff*2, maxBackoff)
		}
	}
}

// connect establishes a single RTSP session, consumes packets, and blocks
// until the connection is lost or context is cancelled.
func (s *RTSPSource) connect(ctx context.Context) (bool, error) {
	u, err := base.ParseURL(s.url)
	if err != nil {
		return false, fmt.Errorf("parse rtsp url: %w", err)
	}

	// Use TCP transport for reliability over WiFi
	tcp := gortsplib.ProtocolTCP
	client := &gortsplib.Client{
		Scheme:   u.Scheme,
		Host:     u.Host,
		Protocol: &tcp,
	}

	if err := client.Start(); err != nil {
		return false, fmt.Errorf("rtsp connect: %w", err)
	}
	defer client.Close()

	desc, _, err := client.Describe(u)
	if err != nil {
		return false, fmt.Errorf("rtsp describe: %w", err)
	}

	// Find H.264 track
	var forma *format.H264
	medi := desc.FindFormat(&forma)
	if medi == nil {
		return false, fmt.Errorf("no h264 track found in rtsp stream")
	}

	// Create RTP decoder
	rtpDec, err := forma.CreateDecoder()
	if err != nil {
		return false, fmt.Errorf("create rtp decoder: %w", err)
	}

	// Setup the media track
	if _, err := client.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		return false, fmt.Errorf("rtsp setup: %w", err)
	}

	// Track timestamp baseline
	var ptsOK bool
	var ptsBase int64
	firstRA := false
	var framesReceived atomic.Bool

	// Packet handler: depacketize RTP → H.264 NALUs → Frame
	client.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
		pts, ok := client.PacketPTS(medi, pkt)
		if !ok {
			return
		}
		if !ptsOK {
			ptsBase = pts
			ptsOK = true
		}
		elapsed := time.Duration(pts-ptsBase) * time.Millisecond

		au, err := rtpDec.Decode(pkt)
		if err != nil {
			if err != rtph264.ErrNonStartingPacketAndNoPrevious && err != rtph264.ErrMorePacketsNeeded {
				slog.Warn("camera: rtsp rtp decode error", "error", err)
			}
			return
		}

		// Wait for first keyframe
		if !firstRA && !h264.IsRandomAccess(au) {
			return
		}
		firstRA = true
		framesReceived.Store(true)

		// Build Annex-B bytestream from NALU access unit
		naluData := make([]byte, 0, 64*1024)
		for _, nalu := range au {
			naluData = append(naluData, []byte{0x00, 0x00, 0x00, 0x01}...)
			naluData = append(naluData, nalu...)
		}

		if len(naluData) == 0 {
			return
		}

		frame := Frame{
			Data:      naluData,
			Timestamp: time.Now(),
			PTS:       int64(elapsed.Seconds() * 90000),
		}

		// Non-blocking send
		s.mu.RLock()
		ch := s.framesCh
		s.mu.RUnlock()
		if ch == nil {
			return
		}
		select {
		case ch <- frame:
		default:
		}
	})

	// Start playback
	if _, err := client.Play(nil); err != nil {
		return false, fmt.Errorf("rtsp play: %w", err)
	}

	slog.Info("camera: rtsp source connected", "url", s.url)

	// Reconnect backoff reset on successful connection
	// Wait until fatal error or context cancel
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Wait()
	}()

	select {
	case <-ctx.Done():
		client.Close()
		return framesReceived.Load(), ctx.Err()
	case err := <-errCh:
		return framesReceived.Load(), err
	}
}
