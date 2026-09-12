// Package rtsp provides an RTSP server for H.264 streaming using gortsplib v5.
package rtsp

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/h264"
)

// Config holds RTSP server configuration.
type Config struct {
	// Port is the RTSP listening port (default: 8554).
	Port int
	// Username for digest auth. Empty means no auth.
	Username string
	// Password for digest auth.
	Password string
	// Address is the local IP advertised in RTSP URLs (default: auto-detect).
	Address string
	// WriteQueueSize is the gortsplib write queue size (default: 2048).
	WriteQueueSize int
	// EnableUDP enables UDP transport (requires UDPRTPPort and UDPRTCPPort).
	EnableUDP bool
	// UDPRTPPort is the UDP port for RTP packets (default: 8000).
	UDPRTPPort int
	// UDPRTCPPort is the UDP port for RTCP packets (default: 8001).
	UDPRTCPPort int
}
// Server wraps gortsplib for H.264 streaming.
// It reads H.264 access units from a frame source channel and
// distributes them as RTP packets to connected RTSP clients.
type Server struct {
	cfg        Config
	rtspServer *gortsplib.Server
	stream     *gortsplib.ServerStream
	media      *description.Media
	h264Format *format.H264
	rtpEncoder *rtph264.Encoder

	mu           sync.Mutex
	frameSource  <-chan h264.AccessUnit
	clientCount  int
	baseTime     time.Time // reference time for PTS calculation
	cancelStream context.CancelFunc
	wg           sync.WaitGroup
}

// New creates a new RTSP server instance. Call Start() to begin listening.
func New(cfg Config) *Server {
	if cfg.Port == 0 {
		cfg.Port = 8554
	}
	if cfg.WriteQueueSize == 0 {
		cfg.WriteQueueSize = 2048
	}

	h264Fmt := &format.H264{
		PayloadTyp:        96,
		PacketizationMode: 1,
	}

	return &Server{
		cfg:        cfg,
		h264Format: h264Fmt,
	}
}

// Start begins listening for RTSP connections.
func (s *Server) Start(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", s.cfg.Port)
	s.media = &description.Media{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{s.h264Format},
	}

	s.rtspServer = &gortsplib.Server{
		Handler:        s,
		RTSPAddress:    addr,
		WriteQueueSize: s.cfg.WriteQueueSize, // 256 default too small for WiFi clients; 2048 ≈ 27s buffer at 75 pkt/s
	}

	// Enable UDP transport if configured (needed for NVR clients that prefer UDP)
	if s.cfg.EnableUDP {
		rtpPort := s.cfg.UDPRTPPort
		if rtpPort == 0 {
			rtpPort = 8000
		}
		rtcpPort := s.cfg.UDPRTCPPort
		if rtcpPort == 0 {
			rtcpPort = 8001
		}
		s.rtspServer.UDPRTPAddress = fmt.Sprintf(":%d", rtpPort)
		s.rtspServer.UDPRTCPAddress = fmt.Sprintf(":%d", rtcpPort)
	}

	// Start the server synchronously — this binds the port and returns immediately
	if err := s.rtspServer.Start(); err != nil {
		return fmt.Errorf("start rtsp server: %w", err)
	}

	// Wait for server to be ready (Start returns quickly, Wait blocks)
	go func() {
		if err := s.rtspServer.Wait(); err != nil {
			log.Printf("rtsp server exited: %v", err)
		}
	}()
	// Wait for server to be ready (check TCP connection)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("rtsp server startup canceled: %w", ctx.Err())
		case <-deadline:
			return fmt.Errorf("rtsp server failed to start within 5s")
		default:
			conn, err := net.DialTimeout("tcp",
				net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", s.cfg.Port)),
				100*time.Millisecond)
			if err == nil {
				conn.Close()
				return nil
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}
// Stop gracefully stops the RTSP server and closes all client connections.
func (s *Server) Stop() error {
	s.mu.Lock()
	if s.cancelStream != nil {
		s.cancelStream()
		s.cancelStream = nil
	}
	s.mu.Unlock()

	s.wg.Wait()

	if s.stream != nil {
		s.stream.Close()
		s.stream = nil
	}

	if s.rtspServer != nil {
		s.rtspServer.Close()
	}
	return nil
}

// SetFrameSource connects a channel of H.264 access units for streaming.
// The server starts consuming frames only when at least one client is connected.
func (s *Server) SetFrameSource(ch <-chan h264.AccessUnit) {
	s.mu.Lock()
	s.frameSource = ch
	s.mu.Unlock()
}

// Port returns the configured RTSP port.
func (s *Server) Port() int {
	return s.cfg.Port
}

// ClientCount returns the current number of connected RTSP clients.
func (s *Server) ClientCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientCount
}

// --- gortsplib.ServerHandler interface ---

// OnConnOpen is called when a new RTSP connection is opened.
func (s *Server) OnConnOpen(_ *gortsplib.ServerHandlerOnConnOpenCtx) {
}

// OnConnClose is called when an RTSP connection is closed.
func (s *Server) OnConnClose(_ *gortsplib.ServerHandlerOnConnCloseCtx) {
}

// OnStreamWriteError handles per-packet write errors (e.g., "write queue is full").
// Without this, gortsplib falls back to log.Println for EVERY dropped packet —
// causing thousands of log lines per minute when a client is slow or disconnected.
// We silently drop: the server's ReadTimeout (10s) will clean up zombie connections.
func (s *Server) OnStreamWriteError(_ *gortsplib.ServerHandlerOnStreamWriteErrorCtx) {
}

// OnSessionOpen is called when a new RTSP session is opened.
func (s *Server) OnSessionOpen(_ *gortsplib.ServerHandlerOnSessionOpenCtx) {
}

// OnSessionClose is called when a session is closed.
func (s *Server) OnSessionClose(_ *gortsplib.ServerHandlerOnSessionCloseCtx) {
	s.mu.Lock()
	s.clientCount--
	if s.clientCount == 0 {
		s.stopFrameReader()
	}
	s.mu.Unlock()
}

// OnDescribe handles RTSP DESCRIBE requests.
// Returns the stream description with H.264 media.
func (s *Server) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (
	*base.Response, *gortsplib.ServerStream, error,
) {
	if s.hasAuth() {
		if !ctx.Conn.VerifyCredentials(ctx.Request, s.cfg.Username, s.cfg.Password) {
			return &base.Response{StatusCode: base.StatusUnauthorized}, nil, liberrors.ErrServerAuth{}
		}
	}

	s.mu.Lock()
	if s.stream == nil {
		s.initStream()
	}
	stream := s.stream
	s.mu.Unlock()

	if stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}

	return &base.Response{StatusCode: base.StatusOK}, stream, nil
}

// OnSetup handles RTSP SETUP requests.
func (s *Server) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (
	*base.Response, *gortsplib.ServerStream, error,
) {
	if s.hasAuth() {
		if !ctx.Conn.VerifyCredentials(ctx.Request, s.cfg.Username, s.cfg.Password) {
			return &base.Response{StatusCode: base.StatusUnauthorized}, nil, liberrors.ErrServerAuth{}
		}
	}

	s.mu.Lock()
	if s.stream == nil {
		s.initStream()
	}
	stream := s.stream
	s.mu.Unlock()

	return &base.Response{StatusCode: base.StatusOK}, stream, nil
}

// OnPlay handles RTSP PLAY requests.
// Starts frame consumption when the first client starts playing.
func (s *Server) OnPlay(_ *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	s.mu.Lock()
	s.clientCount++
	if s.clientCount == 1 {
		s.startFrameReader()
	}
	s.mu.Unlock()

	return &base.Response{StatusCode: base.StatusOK}, nil
}

// --- internal helpers ---

func (s *Server) hasAuth() bool {
	return s.cfg.Username != ""
}

func (s *Server) initStream() {
	desc := &description.Session{
		Medias: []*description.Media{s.media},
	}
	s.stream = &gortsplib.ServerStream{
		Server: s.rtspServer,
		Desc:   desc,
	}
	if err := s.stream.Initialize(); err != nil {
		log.Printf("rtsp: failed to initialize stream: %v", err)
		s.stream = nil
		return
	}

	// Create RTP encoder if not already created
	if s.rtpEncoder == nil {
		enc, err := s.h264Format.CreateEncoder()
		if err != nil {
			log.Printf("rtsp: failed to create RTP encoder: %v", err)
			return
		}
		s.rtpEncoder = enc
	}
}

func (s *Server) startFrameReader() {
	s.baseTime = time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	s.cancelStream = cancel

	s.wg.Add(1)
	go s.readFrames(ctx)
}

func (s *Server) stopFrameReader() {
	if s.cancelStream != nil {
		s.cancelStream()
		s.cancelStream = nil
	}
}

func (s *Server) readFrames(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case au, ok := <-s.frameSource:
			if !ok {
				return
			}
			s.processAccessUnit(au)
		}
	}
}

func (s *Server) processAccessUnit(au h264.AccessUnit) {
	s.mu.Lock()
	stream := s.stream
	encoder := s.rtpEncoder
	s.mu.Unlock()

	if stream == nil || encoder == nil {
		return
	}

	// Convert NALUs to [][]byte
	nalus := make([][]byte, 0, len(au.NALUs)+2)
	hasIDR := false
	hasSPS := false
	hasPPS := false
	for _, nalu := range au.NALUs {
		nalus = append(nalus, nalu.Data)
		if nalu.IsIDR {
			hasIDR = true
		}
		if nalu.IsSPS {
			hasSPS = true
		}
		if nalu.IsPPS {
			hasPPS = true
		}
	}
	if len(nalus) == 0 {
		return
	}

	// Check for SPS/PPS in this access unit — update format if needed
	for _, nalu := range au.NALUs {
		if nalu.IsSPS || nalu.IsPPS {
			s.updateFormat(au.NALUs)
			break
		}
	}

	// Inject SPS+PPS before IDR frames that don't already include them.
	// This ensures late-joining RTSP clients can decode the stream without
	// waiting for the next keyframe with embedded SPS/PPS.
	if hasIDR && (!hasSPS || !hasPPS) {
		s.mu.Lock()
		spsData := s.h264Format.SPS
		ppsData := s.h264Format.PPS
		s.mu.Unlock()

		if spsData != nil && ppsData != nil {
			injected := make([][]byte, 0, len(nalus)+2)
			if !hasSPS {
				injected = append(injected, spsData)
			}
			if !hasPPS {
				injected = append(injected, ppsData)
			}
			injected = append(injected, nalus...)
			nalus = injected
		}
	}

	// Encode into RTP packets
	pkts, err := encoder.Encode(nalus)
	if err != nil {
		log.Printf("rtsp: RTP encode error: %v", err)
		return
	}

	// Calculate RTP timestamp from time.Time (90kHz clock)
	pts := uint32(au.Timestamp.Sub(s.baseTime) * time.Duration(90000) / time.Second)

	// Write RTP packets
	for _, pkt := range pkts {
		pkt.Timestamp = pts
		if err := stream.WritePacketRTP(s.media, pkt); err != nil {
			// Stream may have been closed
			return
		}
	}
}

func (s *Server) updateFormat(nalus []h264.NALU) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var sps, pps []byte
	for _, nalu := range nalus {
		if nalu.IsSPS && sps == nil {
			sps = nalu.Data
		}
		if nalu.IsPPS && pps == nil {
			pps = nalu.Data
		}
	}

	if sps != nil && pps != nil {
		s.h264Format.SPS = sps
		s.h264Format.PPS = pps

		// Re-initialize stream with updated format only if no clients connected
		if s.stream != nil && s.clientCount == 0 {
			s.stream.Close()
			s.initStream()
		}
	}
}
