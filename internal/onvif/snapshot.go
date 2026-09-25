package onvif

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/xiqing85/mibee-eye-go/internal/h264"
)

// ---------------------------------------------------------------------------
// SnapshotBuffer — stores the latest H.264 IDR frame for snapshot capture
// ---------------------------------------------------------------------------

// SnapshotBuffer stores the latest H.264 IDR frame from the AUHub stream
// and provides JPEG capture via rpicam-still (with H.264 fallback).
type SnapshotBuffer struct {
	mu        sync.RWMutex
	latestIDR []byte // raw H.264 IDR NALU with start code
	latestSPS []byte // SPS NALU with start code
	latestPPS []byte // PPS NALU with start code
	hasFrame  bool
	enabled   bool
	stillBin  string // still-capture binary (camera.still_bin)
	ffmpegBin string // transcode binary (camera.ffmpeg_bin)
	// Device transform baked into the stream (SPEC appendix A #9/#19) —
	// tier-1 rpicam-still must apply the same flags or its JPEGs would
	// mismatch the (transformed) stream. Set before Start via SetTransform.
	rotation int
	hflip    bool
	vflip    bool
}

// NewSnapshotBuffer creates a new SnapshotBuffer. The binaries come from
// camera config (still_bin / ffmpeg_bin) — no defaults are invented here.
func NewSnapshotBuffer(enabled bool, stillBin, ffmpegBin string) *SnapshotBuffer {
	return &SnapshotBuffer{enabled: enabled, stillBin: stillBin, ffmpegBin: ffmpegBin}
}

// SetTransform pins the device-level transform (SPEC appendix A #9/#19)
// so tier-1 rpicam-still snapshots match the stream orientation. Call
// before the buffer serves requests.
func (sb *SnapshotBuffer) SetTransform(rotation int, hflip, vflip bool) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.rotation = rotation
	sb.hflip = hflip
	sb.vflip = vflip
}

// stillArgs builds the transform-carrying tail of the rpicam-still argv.
func (sb *SnapshotBuffer) stillArgs() []string {
	var args []string
	if sb.rotation != 0 {
		args = append(args, "--rotation", fmt.Sprintf("%d", sb.rotation))
	}
	if sb.hflip {
		args = append(args, "--hflip")
	}
	if sb.vflip {
		args = append(args, "--vflip")
	}
	return args
}

// Update stores the latest IDR frame data from an AUHub access unit.
// Called from the AUHub subscriber goroutine.
func (sb *SnapshotBuffer) Update(au h264.AccessUnit) {
	if !sb.enabled {
		return
	}
	if !au.KeyFrame || len(au.NALUs) == 0 {
		return
	}

	sb.mu.Lock()
	defer sb.mu.Unlock()

	// Reset
	sb.latestIDR = nil
	sb.latestSPS = nil
	sb.latestPPS = nil

	startCode := []byte{0x00, 0x00, 0x00, 0x01}

	for _, nalu := range au.NALUs {
		if nalu.IsSPS {
			sb.latestSPS = append(startCode, nalu.Data...)
		}
		if nalu.IsPPS {
			sb.latestPPS = append(startCode, nalu.Data...)
		}
		if nalu.IsIDR {
			sb.latestIDR = append(startCode, nalu.Data...)
		}
	}

	sb.hasFrame = sb.latestIDR != nil
}

// Snapshot returns a JPEG image or raw H.264 IDR frame.
//
// Strategy (three-tier, issue mibee-eye-raspi#26):
//  1. Try rpicam-still subprocess for a real JPEG (works only when the
//     camera is idle — under the rpicamvid streaming mode rpicam-vid owns
//     /dev/video0 exclusively, so this tier never wins while streaming).
//  2. Transcode the stored H.264 access unit (SPS+PPS+IDR) to a single
//     JPEG frame via ffmpeg — always available once frames flow, and the
//     honest image/jpeg the ONVIF GetSnapshotUri consumer expects.
//  3. Fall back to the raw H.264 access unit with Content-Type video/H264
//     (ffmpeg missing or the IDR undecodable — explicit for consumers).
//
// Returns: image bytes, MIME content type, error.
func (sb *SnapshotBuffer) Snapshot() ([]byte, string, error) {
	// Tier 1: still-capture JPEG (camera.still_bin)
	if data, err := sb.captureStill(); err == nil {
		slog.Debug("snapshot: captured via still bin", "bin", sb.stillBin)
		return data, "image/jpeg", nil
	}

	sb.mu.RLock()
	defer sb.mu.RUnlock()

	if !sb.hasFrame {
		return nil, "", fmt.Errorf("no frame available")
	}

	// Build complete H.264 access unit: SPS + PPS + IDR
	var buf bytes.Buffer
	if sb.latestSPS != nil {
		buf.Write(sb.latestSPS)
	}
	if sb.latestPPS != nil {
		buf.Write(sb.latestPPS)
	}
	buf.Write(sb.latestIDR)

	// Tier 2: single-frame transcode of the cached access unit
	// (camera.ffmpeg_bin).
	if data, err := sb.h264ToJPEG(buf.Bytes()); err == nil {
		slog.Debug("snapshot: cached IDR transcoded to JPEG via ffmpeg")
		return data, "image/jpeg", nil
	} else {
		// Not silent: a tier-2 failure drops the consumer to raw H.264 —
		// say why so the device can tell us (found live: 3s was too
		// tight for cold ffmpeg on a loaded Pi 3B, ~2.4s measured).
		slog.Warn("snapshot: IDR transcode to JPEG failed, falling back to raw H.264", "error", err)
	}

	// Tier 3: raw access unit, honest content type.
	return buf.Bytes(), "video/H264", nil
}

// HasFrame returns true if an IDR frame is available.
func (sb *SnapshotBuffer) HasFrame() bool {
	sb.mu.RLock()
	defer sb.mu.RUnlock()
	return sb.hasFrame
}

// Enabled returns whether the snapshot buffer is active.
func (sb *SnapshotBuffer) Enabled() bool {
	return sb.enabled
}

// SubscribeToHub subscribes to the AUHub and updates the snapshot buffer
// whenever a key frame (IDR) is received. Blocks until ctx is cancelled.
func (sb *SnapshotBuffer) SubscribeToHub(ctx context.Context, hub *h264.AUHub) {
	if !sb.enabled {
		return
	}
	sub := hub.Subscribe(ctx)
	slog.Debug("snapshot: subscribed to AUHub")
	for {
		select {
		case <-ctx.Done():
			return
		case au, ok := <-sub.Channel:
			if !ok {
				return
			}
			sb.Update(au)
		}
	}
}

// captureStill attempts to capture a JPEG frame via the configured
// still-capture binary (camera.still_bin, e.g. rpicam-still).
// Returns the JPEG bytes on success, or an error if the camera is busy/unavailable.
func (sb *SnapshotBuffer) captureStill() ([]byte, error) {
	if sb.stillBin == "" {
		return nil, fmt.Errorf("still bin not configured (camera.still_bin)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	sb.mu.RLock()
	transformArgs := sb.stillArgs()
	sb.mu.RUnlock()
	cmdArgs := []string{
		"-o", "-",
		"--nopreview",
		"-t", "100", // 100ms timeout — fail fast if camera is busy
	}
	cmdArgs = append(cmdArgs, transformArgs...)
	cmd := exec.CommandContext(ctx, sb.stillBin, cmdArgs...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("still bin %q: %w", sb.stillBin, err)
	}

	data := stdout.Bytes()
	if len(data) < 100 {
		return nil, fmt.Errorf("still bin %q: output too small (%d bytes)", sb.stillBin, len(data))
	}

	// Verify JPEG header (SOI marker 0xFFD8)
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, fmt.Errorf("still bin %q: output is not JPEG (got 0x%02x 0x%02x)", sb.stillBin, data[0], data[1])
	}

	return data, nil
}

// transcodeArgs builds the ffmpeg argv for the tier-2 IDR→JPEG transcode:
// Annex-B AU on stdin, single JPEG frame on stdout. The output muxer must
// be pipe-safe — `-f image2` writing to pipe:1 while reading pipe:0 hangs
// ffmpeg 7.1.5 (deb13, the Pi image) forever; only the device shows it.
func (sb *SnapshotBuffer) transcodeArgs() []string {
	return []string{
		"-loglevel", "error",
		"-f", "h264", "-i", "pipe:0",
		"-frames:v", "1", "-q:v", "3",
		"-f", "mjpeg", "pipe:1",
	}
}

// h264ToJPEG transcodes one Annex-B H.264 access unit into a single JPEG
// frame via the ffmpeg binary (already a device dependency for the AI
// keyframe decoder). Errors leave the caller to the raw-IDR tier.
func (sb *SnapshotBuffer) h264ToJPEG(annexB []byte) ([]byte, error) {
	if sb.ffmpegBin == "" {
		return nil, fmt.Errorf("ffmpeg bin not configured (camera.ffmpeg_bin)")
	}
	// Cold ffmpeg start on a fully-loaded Pi 3B measures ~2.4s just for
	// spawn+decode of one frame; 3s dropped tier 2 under load spikes.
	// Snapshots are rare, user/NVR-triggered operations — the client is
	// waiting either way, so budget generously.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, sb.ffmpegBin, sb.transcodeArgs()...)
	cmd.Stdin = bytes.NewReader(annexB)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg h264->jpeg: %w", err)
	}

	data := stdout.Bytes()
	if len(data) < 100 {
		return nil, fmt.Errorf("ffmpeg h264->jpeg: output too small (%d bytes)", len(data))
	}
	if data[0] != 0xFF || data[1] != 0xD8 {
		return nil, fmt.Errorf("ffmpeg h264->jpeg: output is not JPEG (got 0x%02x 0x%02x)", data[0], data[1])
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// HTTP handler — GET /snapshot
// ---------------------------------------------------------------------------

// ServeHTTP handles GET /snapshot requests.
// Returns a JPEG image with Content-Type: image/jpeg, or a raw H.264 frame
// with Content-Type: video/H264 as fallback.
func (sb *SnapshotBuffer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data, contentType, err := sb.Snapshot()
	if err != nil {
		slog.Warn("snapshot: failed to capture", "error", err)
		http.Error(w, "snapshot unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Write(data)
}
