package web

// Chunked-HTTP fMP4 streaming for MSE (SPEC v1 §4.1:
// GET /api/cameras/{id}/stream.mse). Replaces the WebSocket video path:
// init segment first, then one moof+mdat fragment per access unit, with the
// same SPS/PPS caching + fast-forward strategies the WS handler had.

import (
	"net/http"
	"sync"
	"time"

	"github.com/xiqing85/mibee-eye-go/internal/h264"
)

// mseClock hands the fMP4 media timeline from one HTTP connection to the
// next. Timestamps never restart at zero: a new connection seeds strictly
// past every timestamp any earlier connection emitted, so a client that
// transparently reconnects (Wi-Fi blip, proxy idle cut) can keep appending
// to its existing SourceBuffer instead of tearing the decoder down — the
// seamless-reconnect contract (SPEC §4.1).
var (
	mseClockMu    sync.Mutex
	mseFloorTicks uint64
)

// seedMseClock returns the first timestamp for a new connection.
func seedMseClock() uint64 {
	mseClockMu.Lock()
	defer mseClockMu.Unlock()
	mseFloorTicks += 90
	return mseFloorTicks
}

// publishMseClock advances the shared floor so later connections seed past
// this one. No-op when ticks is behind a connection that raced ahead.
func publishMseClock(ticks uint64) {
	mseClockMu.Lock()
	defer mseClockMu.Unlock()
	if ticks > mseFloorTicks {
		mseFloorTicks = ticks
	}
}

// mseTimeline stamps one connection's frames on the shared media clock.
type mseTimeline struct {
	prev  time.Time
	clock uint64
}

func newMseTimeline() *mseTimeline {
	return &mseTimeline{clock: seedMseClock()}
}

// next returns this frame's timestamp (90 kHz ticks) and its duration.
// Wall-clock interval → ticks keeps the timeline gapless and true-speed
// regardless of sensor fps; long stalls clamp to 200 ms so server-side
// realignment skips stay seamless for the decoder.
func (m *mseTimeline) next(now time.Time) (timestamp, duration uint64) {
	ticks := uint64(6000)
	if !m.prev.IsZero() {
		us := now.Sub(m.prev).Microseconds() * 90 / 1000
		if us < 90 {
			us = 90
		} else if us > 18000 {
			us = 18000
		}
		ticks = uint64(us)
	}
	m.prev = now
	m.clock += ticks
	publishMseClock(m.clock)
	return m.clock, ticks
}

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

// clearWriteDeadline opts a streaming response out of the server's global
// http.Server.WriteTimeout (default 30s). Chunked streams (MSE, SSE) are
// long-lived by design; leaving the deadline in place cuts them off 30s
// into the response, which the live player surfaces as a black-flash
// reconnect every 30 seconds.
func clearWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
}

// extendReadDeadline gives a large-body upload (AI model, up to tens of
// MB) room to arrive on a slow link: the server's ReadTimeout (default
// 10s) covers the whole request including the body, so an upload slower
// than that would be killed mid-flight no matter how small the model is.
// Bounded rather than cleared — a hung client still gets its goroutine
// back eventually.
func extendReadDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(10 * time.Minute))
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
	clearWriteDeadline(w)

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	sub := s.cfg.AUHub.Subscribe(r.Context())
	defer s.cfg.AUHub.Unsubscribe(sub.ID)

	var cachedSPS, cachedPPS []byte
	initialized := false
	rt := realignTracker{seenDrops: sub.Dropped()}
	var sequence uint32
	tl := newMseTimeline()

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
		// and true-speed regardless of sensor fps; the shared clock hands
		// the timeline to the client's next (re)connection.
		timestamp, duration := tl.next(time.Now())

		seg := buildMediaSegment(nalus, sequence, timestamp, uint32(duration), au.KeyFrame)
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
		return uint32(oc.CameraEffectiveWidth()), uint32(oc.CameraEffectiveHeight())
	}
	return 1280, 720
}

// compile-time assertion that the hub types stay wired as expected.
var _ = h264.AccessUnit{}
