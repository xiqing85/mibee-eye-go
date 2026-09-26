// Package rtsp provides an RTSP server for H.264 streaming using gortsplib v5.
package rtsp

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"

	"github.com/xiqing85/mibee-eye-go/internal/h264"
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

// mount is one served stream: the session-owned gortsplib stream plus its
// own media, RTP encoder and frame source. The main mount serves every
// URL (fail-open, the historical single-stream behavior); the sub mount
// serves the exact `/sub` path segment (SPEC appendix A #20).
type mount struct {
	stream     *gortsplib.ServerStream
	media      *description.Media
	format     *format.H264
	rtpEncoder *rtph264.Encoder

	frameSource <-chan h264.AccessUnit
	clientCount int
	baseTime    time.Time
	cancel      context.CancelFunc
}

// Server wraps gortsplib for H.264 streaming.
// It reads H.264 access units from a frame source channel and
// distributes them as RTP packets to connected RTSP clients.
type Server struct {
	cfg        Config
	rtspServer *gortsplib.Server

	mu   sync.Mutex
	main *mount // primary stream (any URL)
	sub  *mount // low-resolution substream (RTSP /sub)

	// Sessions map onto their mount so PLAY/TEARDOWN accounting hits the
	// right stream (SetSetup records it).
	sessions map[*gortsplib.ServerSession]*mount

	wg sync.WaitGroup
}

// New creates a new RTSP server instance. Call Start() to begin listening.
func New(cfg Config) *Server {
	if cfg.Port == 0 {
		cfg.Port = 8554
	}
	if cfg.WriteQueueSize == 0 {
		cfg.WriteQueueSize = 2048
	}

	return &Server{
		cfg: cfg,
		main: &mount{
			format: &format.H264{PayloadTyp: 96, PacketizationMode: 1},
		},
		sub: &mount{
			format: &format.H264{PayloadTyp: 96, PacketizationMode: 1},
		},
		sessions: map[*gortsplib.ServerSession]*mount{},
	}
}

// Start begins listening for RTSP connections.
func (s *Server) Start(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", s.cfg.Port)
	for _, m := range []*mount{s.main, s.sub} {
		m.media = &description.Media{
			Type:    description.MediaTypeVideo,
			Formats: []format.Format{m.format},
		}
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
	for _, m := range []*mount{s.main, s.sub} {
		if m.cancel != nil {
			m.cancel()
			m.cancel = nil
		}
	}
	s.mu.Unlock()

	s.wg.Wait()

	s.mu.Lock()
	for _, m := range []*mount{s.main, s.sub} {
		if m.stream != nil {
			m.stream.Close()
			m.stream = nil
		}
	}
	s.mu.Unlock()

	if s.rtspServer != nil {
		s.rtspServer.Close()
	}
	return nil
}

// SetFrameSource connects a channel of H.264 access units for streaming.
// The server starts consuming frames only when at least one client is connected.
func (s *Server) SetFrameSource(ch <-chan h264.AccessUnit) {
	s.mu.Lock()
	s.main.frameSource = ch
	s.mu.Unlock()
}

// SetSubFrameSource connects the substream access-unit channel (RTSP /sub
// mount, SPEC appendix A #20).
func (s *Server) SetSubFrameSource(ch <-chan h264.AccessUnit) {
	s.mu.Lock()
	s.sub.frameSource = ch
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
	return s.main.clientCount + s.sub.clientCount
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
func (s *Server) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.sessions[ctx.Session]
	if !ok {
		return
	}
	delete(s.sessions, ctx.Session)
	m.clientCount--
	if m.clientCount == 0 {
		s.stopFrameReaderLocked(m)
	}
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

	m := s.mountForURL(ctx.Request.URL)
	stream := s.streamFor(m)
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

	m := s.mountForURL(ctx.Request.URL)
	stream := s.streamFor(m)
	if stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}

	s.mu.Lock()
	s.sessions[ctx.Session] = m
	s.mu.Unlock()

	return &base.Response{StatusCode: base.StatusOK}, stream, nil
}

// OnPlay handles RTSP PLAY requests.
// Starts frame consumption when the first client starts playing.
func (s *Server) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	s.mu.Lock()
	m := s.sessions[ctx.Session]
	if m == nil {
		m = s.mountForURL(ctx.Request.URL)
	}
	m.clientCount++
	if m.clientCount == 1 {
		s.startFrameReaderLocked(m)
	}
	s.mu.Unlock()

	return &base.Response{StatusCode: base.StatusOK}, nil
}

// --- internal helpers ---

func (s *Server) hasAuth() bool {
	return s.cfg.Username != ""
}

// mountForURL resolves the mount for a request URL: an exact `/sub` path
// segment selects the substream; everything else (including query
// strings and unknown paths) resolves to the main stream — legacy
// clients that pass arbitrary URLs keep the historical behavior.
func (s *Server) mountForURL(u *base.URL) *mount {
	if u == nil {
		return s.main
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if strings.EqualFold(seg, "sub") {
			return s.sub
		}
	}
	return s.main
}

// streamFor lazily initializes and returns the mount's ServerStream.
func (s *Server) streamFor(m *mount) *gortsplib.ServerStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.stream == nil {
		s.initStreamLocked(m)
	}
	return m.stream
}

func (s *Server) initStreamLocked(m *mount) {
	desc := &description.Session{
		Medias: []*description.Media{m.media},
	}
	m.stream = &gortsplib.ServerStream{
		Server: s.rtspServer,
		Desc:   desc,
	}
	if err := m.stream.Initialize(); err != nil {
		log.Printf("rtsp: failed to initialize stream: %v", err)
		m.stream = nil
		return
	}

	if m.rtpEncoder == nil {
		enc, err := m.format.CreateEncoder()
		if err != nil {
			log.Printf("rtsp: failed to create RTP encoder: %v", err)
			return
		}
		m.rtpEncoder = enc
	}
}

func (s *Server) startFrameReaderLocked(m *mount) {
	m.baseTime = time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	s.wg.Add(1)
	go s.readFrames(ctx, m)
}

func (s *Server) stopFrameReaderLocked(m *mount) {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}

func (s *Server) readFrames(ctx context.Context, m *mount) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case au, ok := <-m.frameSource:
			if !ok {
				return
			}
			s.processAccessUnit(au, m)
		}
	}
}

func (s *Server) processAccessUnit(au h264.AccessUnit, m *mount) {
	s.mu.Lock()
	stream := m.stream
	encoder := m.rtpEncoder
	media := m.media
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
			s.updateFormat(au.NALUs, m)
			break
		}
	}

	// Inject SPS+PPS before IDR frames that don't already include them.
	// This ensures late-joining RTSP clients can decode the stream without
	// waiting for the next keyframe with embedded SPS/PPS.
	if hasIDR && (!hasSPS || !hasPPS) {
		s.mu.Lock()
		spsData := m.format.SPS
		ppsData := m.format.PPS
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
	pts := uint32(au.Timestamp.Sub(m.baseTime) * time.Duration(90000) / time.Second)

	// Write RTP packets
	for _, pkt := range pkts {
		pkt.Timestamp = pts
		if err := stream.WritePacketRTP(media, pkt); err != nil {
			// Stream may have been closed
			return
		}
	}
}

func (s *Server) updateFormat(nalus []h264.NALU, m *mount) {
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
		m.format.SPS = sps
		m.format.PPS = pps

		// Re-initialize stream with updated format only if no clients connected
		if m.stream != nil && m.clientCount == 0 {
			m.stream.Close()
			s.initStreamLocked(m)
		}
	}
}
