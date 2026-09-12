package web

// Chunked-HTTP fMP4 streaming for MSE (SPEC v1 §4.1:
// GET /api/cameras/{id}/stream.mse). Replaces the WebSocket video path:
// init segment first, then one moof+mdat fragment per access unit, with the
// same SPS/PPS caching + fast-forward strategies the WS handler had.

import (
	"net/http"
	"time"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/h264"
)

// realignTracker gates serialization after stream loss. A unit lost to a
// full subscriber channel (or skipped while draining backlog) leaves a
// reference-frame hole; continuing with dependent frames freezes decoders
// until an IDR arrives. The tracker blocks everything but keyframes until
// one realigns the sequence.
type realignTracker struct {
	needKey   bool
	seenDrops uint64
}

// allow reports whether au may be serialized. dropped is the subscriber's
// current drop counter; drained is true when the handler fast-forwarded
// through backlog and landed on a non-keyframe (a skipped range).
func (rt *realignTracker) allow(au h264.AccessUnit, dropped uint64, drained bool) bool {
	if dropped != rt.seenDrops {
		rt.seenDrops = dropped
		rt.needKey = true
	}
	if drained {
		rt.needKey = true
	}
	if rt.needKey && !au.KeyFrame {
		return false
	}
	rt.needKey = false
	return true
}

// handleStreamMSE streams H.264 as fMP4 over chunked HTTP.
func (s *Server) handleStreamMSE(w http.ResponseWriter, r *http.Request, cameraID string) {
	if cameraID != "0" {
		writeError(w, http.StatusNotFound, "no such camera")
		return
	}
	if s.cfg.AUHub == nil {
		http.Error(w, "streaming not available", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	sub := s.cfg.AUHub.Subscribe(r.Context())
	defer s.cfg.AUHub.Unsubscribe(sub.ID)

	var cachedSPS, cachedPPS []byte
	initialized := false
	rt := realignTracker{seenDrops: sub.Dropped()}
	var sequence uint32
	var prevFrame time.Time
	var mediaClock uint64

	for au := range sub.Channel {
		select {
		case <-r.Context().Done():
			return
		default:
		}

		// Fast-forward: drain stale units when we fall behind. Landing on a
		// keyframe is a clean jump; landing elsewhere leaves a hole and the
		// realign tracker below waits for the next IDR.
		drainedNonKey := false
		for len(sub.Channel) > 2 {
			candidate := <-sub.Channel
			if candidate.KeyFrame {
				au = candidate
				drainedNonKey = false
				break
			}
			au = candidate
			drainedNonKey = true
		}

		// Update the SPS/PPS cache from whatever this AU carries.
		for _, nalu := range au.NALUs {
			if nalu.IsSPS {
				cachedSPS = nalu.Data
			}
			if nalu.IsPPS {
				cachedPPS = nalu.Data
			}
		}

		if !initialized {
			if !au.KeyFrame || cachedSPS == nil || cachedPPS == nil {
				continue
			}
			width, height := s.cameraDimensions()
			if _, err := w.Write(buildInitSegment(cachedSPS, cachedPPS, width, height)); err != nil {
				return
			}
			flusher.Flush()
			initialized = true
		} else if !rt.allow(au, sub.Dropped(), drainedNonKey) {
			// Loss realignment: wait for an IDR instead of feeding decoders
			// frames whose references were dropped or skipped.
			continue
		}

		nalus := make([][]byte, 0, len(au.NALUs))
		for _, nalu := range au.NALUs {
			nalus = append(nalus, nalu.Data)
		}

		// Wall-clock interval → 90 kHz ticks keeps the MSE timeline gapless
		// and true-speed regardless of sensor fps.
		now := time.Now()
		duration := uint32(6000)
		if !prevFrame.IsZero() {
			us := now.Sub(prevFrame).Microseconds()
			ticks := us * 90 / 1000
			if ticks < 90 {
				ticks = 90
			} else if ticks > 18000 {
				ticks = 18000
			}
			duration = uint32(ticks)
		}
		prevFrame = now
		timestamp := mediaClock
		mediaClock += uint64(duration)

		seg := buildMediaSegment(nalus, sequence, timestamp, duration, au.KeyFrame)
		sequence++
		if _, err := w.Write(seg); err != nil {
			return
		}
		flusher.Flush()
	}
}

// cameraDimensions reports the configured capture size for the init segment.
func (s *Server) cameraDimensions() (uint32, uint32) {
	if oc := s.cfg.OnvifConfig; oc != nil {
		return uint32(oc.CameraWidth()), uint32(oc.CameraHeight())
	}
	return 1280, 720
}

// compile-time assertion that the hub types stay wired as expected.
var _ = h264.AccessUnit{}
