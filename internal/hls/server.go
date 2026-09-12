// HLS live streaming server with pure-Go MPEG-TS segmenter.
//
// Subscribes to the H.264 AUHub, segments access units into MPEG-TS
// segments (via muxts), maintains a sliding window playlist (m3u8),
// and serves both via HTTP.
package hls

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/h264"
)

// Segment holds a complete MPEG-TS segment with its sequence number.
type Segment struct {
	Data     []byte
	Sequence uint64
	Duration time.Duration
}

// Server is the HLS live streaming server.
type Server struct {
	mu sync.Mutex

	hub             *h264.AUHub
	segmentDuration time.Duration
	fps             int        // frames per second for PTS calculation
	windowSize      int        // number of segments to keep (default 5)

	segments []*Segment    // sliding window (oldest at index 0)
	sequence uint64        // next segment sequence number

	// Current segment being built.
	currentSeg *segmentBuilder
	currentPTS int64  // PTS for the next AU (running clock, never resets)
	frameCount int64  // total frames processed (for PTS tracking)

	// Cached SPS/PPS from the latest keyframe (raw NALU data, no start codes).
	latestSPS []byte
	latestPPS []byte

	// Track whether we've started a segment on a keyframe boundary.
	waitingForKeyframe bool

	// Latest playlist (cached for efficiency).
	cachedPlaylist string
}

// Config holds HLS server parameters.
type Config struct {
	Hub             *h264.AUHub
	SegmentDuration time.Duration
	FPS             int // frames per second for PTS calculation
}

// New creates a new HLS Server.
func New(cfg Config) *Server {
	dur := cfg.SegmentDuration
	if dur <= 0 {
		dur = 2 * time.Second
	}
	fps := cfg.FPS
	if fps <= 0 {
		fps = 15
	}
	return &Server{
		hub:               cfg.Hub,
		segmentDuration:   dur,
		fps:               fps,
		windowSize:        5,
		waitingForKeyframe: true,
	}
}

// Start subscribes to AUHub and begins segmenting. Blocks until ctx is done.
func (s *Server) Start(ctx context.Context) {
	sub := s.hub.Subscribe(ctx)
	slog.Info("hls: server started",
		"segment_duration", s.segmentDuration,
		"fps", s.fps)

	// PTS increment per frame at 90kHz clock.
	ptsIncrement := int64(90000 / s.fps)

	for {
		select {
		case <-ctx.Done():
			slog.Info("hls: server stopped")
			return
		case au, ok := <-sub.Channel:
			if !ok {
				return
			}
			s.processAU(au, ptsIncrement)
		}
	}
}
// processAU handles one access unit.
func (s *Server) processAU(au h264.AccessUnit, ptsIncrement int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Extract and cache SPS/PPS from this AU.
	for _, nalu := range au.NALUs {
		if nalu.IsSPS {
			s.latestSPS = append([]byte(nil), nalu.Data...)
		}
		if nalu.IsPPS {
			s.latestPPS = append([]byte(nil), nalu.Data...)
		}
	}

	// If waiting for a keyframe to start the next segment, skip non-IDR.
	if s.waitingForKeyframe {
		if !au.KeyFrame {
			s.currentPTS += ptsIncrement
			return
		}
		s.waitingForKeyframe = false
		s.currentSeg = newSegment()
	}

	// Lazily create segment on first AU (must be keyframe).
	if s.currentSeg == nil {
		if !au.KeyFrame {
			s.currentPTS += ptsIncrement
			return
		}
		s.currentSeg = newSegment()
	}

	// Extract NALU data (without start codes) for the segment builder.
	naluData := make([][]byte, len(au.NALUs))
	for i, nalu := range au.NALUs {
		naluData[i] = nalu.Data
	}

	// Write the access unit to the segment.
	s.currentSeg.writeAU(naluData, s.currentPTS, au.KeyFrame, s.latestSPS, s.latestPPS)
	s.currentPTS += ptsIncrement

	// Check if we should close the segment (based on frame count since start).
	s.frameCount++
	framesPerSegment := s.fps * int(s.segmentDuration.Seconds())
	if framesPerSegment <= 0 {
		framesPerSegment = 30
	}
	if s.frameCount%int64(framesPerSegment) == 0 {
		s.closeSegment()
		s.waitingForKeyframe = true
	}
}

// closeSegment finalises the current segment and adds it to the window.
func (s *Server) closeSegment() {
	if s.currentSeg == nil {
		return
	}

	data := s.currentSeg.bytes()
	if len(data) == 0 {
		return
	}

	// Calculate actual duration from PTS progression.
	// frameCountSinceLastClose * (90000/fps) / 90000
	duration := s.segmentDuration // approximate

	seg := &Segment{
		Data:     data,
		Sequence: s.sequence,
		Duration: duration,
	}
	s.sequence++

	s.segments = append(s.segments, seg)

	// Trim to window size.
	if len(s.segments) > s.windowSize {
		excess := len(s.segments) - s.windowSize
		s.segments = s.segments[excess:]
	}

	s.currentSeg = nil
	s.cachedPlaylist = "" // invalidate cache
	slog.Debug("hls: segment closed",
		"sequence", seg.Sequence,
		"size", len(data),
		"duration", duration)
}

// playlist generates the m3u8 playlist for the current window.
func (s *Server) playlist() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cachedPlaylist != "" {
		return s.cachedPlaylist
	}

	if len(s.segments) == 0 {
		return "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n"
	}

	// Find target duration (max segment duration).
	var maxDur float64
	for _, seg := range s.segments {
		d := seg.Duration.Seconds()
		if d > maxDur {
			maxDur = d
		}
	}
	if maxDur <= 0 {
		maxDur = s.segmentDuration.Seconds()
	}

	// Get the smallest sequence number.
	firstSeq := s.segments[0].Sequence

	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:3\n")
	sb.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%.0f\n", maxDur))
	sb.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", firstSeq))

	for _, seg := range s.segments {
		d := seg.Duration.Seconds()
		if d <= 0 {
			d = s.segmentDuration.Seconds()
		}
		sb.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", d))
		sb.WriteString(fmt.Sprintf("segment-%d.ts\n", seg.Sequence))
	}

	s.cachedPlaylist = sb.String()
	return s.cachedPlaylist
}

// ServePlaylist handles GET /hls/stream.m3u8
func (s *Server) ServePlaylist(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write([]byte(s.playlist()))
}

// ServeSegment handles GET /hls/segment-{N}.ts
func (s *Server) ServeSegment(w http.ResponseWriter, r *http.Request) {
	seqStr := r.PathValue("seq")
	seq, err := strconv.ParseUint(seqStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid sequence number", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	var data []byte
	for _, seg := range s.segments {
		if seg.Sequence == seq {
			data = seg.Data
			break
		}
	}
	s.mu.Unlock()

	if data == nil {
		http.Error(w, "segment not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "video/MP2T")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(data)
}

// RegisterHTTP registers HLS HTTP handlers on the given mux under the /hls/ prefix.
func (s *Server) RegisterHTTP(mux *http.ServeMux) {
	mux.HandleFunc("GET /hls/stream.m3u8", s.ServePlaylist)
	mux.HandleFunc("GET /hls/segment-{seq}.ts", s.ServeSegment)
}

// SegmentCount returns the number of segments in the window.
func (s *Server) SegmentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.segments)
}
