package onvif

import (
	"bytes"
	"os"
	"os/exec"
	"testing"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/h264"
)

// loadSnapshotFixture parses testdata/snapshot_idr.h264 — a real, decodable
// one-frame H.264 stream (SPS+PPS+SEI+IDR, generated with
// `ffmpeg -f lavfi -i color -c:v libx264`) — into a keyframe AccessUnit.
func loadSnapshotFixture(t *testing.T) h264.AccessUnit {
	t.Helper()
	blob, err := os.ReadFile("testdata/snapshot_idr.h264")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var nalus []h264.NALU
	for _, raw := range bytes.Split(blob, []byte{0x00, 0x00, 0x01}) {
		// A 4-byte start code leaves its leading 0x00 on the chunk.
		raw = bytes.TrimLeft(raw, "\x00")
		if len(raw) == 0 {
			continue
		}
		nalu := h264.NALU{Type: raw[0] & 0x1F, Data: raw}
		switch nalu.Type {
		case 5:
			nalu.IsIDR = true
		case 7:
			nalu.IsSPS = true
		case 8:
			nalu.IsPPS = true
		}
		nalus = append(nalus, nalu)
	}
	au := h264.AccessUnit{NALUs: nalus}
	for _, n := range nalus {
		if n.IsIDR {
			au.KeyFrame = true
		}
	}
	if !au.KeyFrame {
		t.Fatal("fixture contains no IDR NALU")
	}
	return au
}

// Issue mibee-eye-raspi#26: under the rpicamvid camera mode rpicam-vid owns
// /dev/video0 exclusively, so the rpicam-still tier can never win — the
// snapshot must still surface as JPEG (the ONVIF GetSnapshotUri semantic) by
// transcoding the cached IDR through ffmpeg.
func TestSnapshotJpegViaFFmpegTranscode(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	if _, err := exec.LookPath("rpicam-still"); err == nil {
		t.Skip("rpicam-still present: tier 1 would win, not exercising the transcode tier")
	}

	sb := NewSnapshotBuffer(true)
	sb.Update(loadSnapshotFixture(t))

	data, contentType, err := sb.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if contentType != "image/jpeg" {
		t.Fatalf("content type = %q, want image/jpeg", contentType)
	}
	if len(data) < 100 || data[0] != 0xFF || data[1] != 0xD8 {
		t.Fatalf("output is not a JPEG (len=%d, head 0x%02x 0x%02x)", len(data), data[0], data[1])
	}
}

// When the transcode tier cannot decode (corrupt IDR) the snapshot falls
// back to the raw IDR with the honest video/H264 content type.
func TestSnapshotFallsBackToRawIDRWhenTranscodeFails(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	if _, err := exec.LookPath("rpicam-still"); err == nil {
		t.Skip("rpicam-still present: tier 1 would win")
	}

	sb := NewSnapshotBuffer(true)
	sb.Update(h264.AccessUnit{
		KeyFrame: true,
		NALUs: []h264.NALU{
			{Type: 7, IsSPS: true, Data: []byte{0x67, 0x42, 0x00, 0x0a}},
			{Type: 8, IsPPS: true, Data: []byte{0x68, 0xce, 0x38, 0x80}},
			{Type: 5, IsIDR: true, Data: []byte{0x65, 0x88, 0x84, 0x00, 0x01}},
		},
	})

	data, contentType, err := sb.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if contentType != "video/H264" {
		t.Fatalf("content type = %q, want video/H264", contentType)
	}
	if !bytes.HasPrefix(data, []byte{0x00, 0x00, 0x00, 0x01, 0x67}) {
		t.Fatalf("raw fallback should start with the SPS start code, got % 02x", data[:5])
	}
}
