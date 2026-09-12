package rtmp

import (
	"testing"
)

func TestParseRTMPURL(t *testing.T) {
	tests := []struct {
		raw          string
		wantAddr     string
		wantApp      string
		wantStreamKey string
	}{
		{"rtmp://example.com/live/stream", "example.com:1935", "live", "stream"},
		{"rtmp://example.com:1935/live/stream", "example.com:1935", "live", "stream"},
		{"rtmp://192.168.1.1/app/key", "192.168.1.1:1935", "app", "key"},
		{"rtmp://example.com:8080/app", "example.com:8080", "app", "stream"},
		{"example.com/live/stream", "example.com:1935", "live", "stream"},
		{"rtmp://host:1935/a/b/c", "host:1935", "a", "b/c"},
	}
	for _, tt := range tests {
		addr, app, key, err := parseRTMPURL(tt.raw)
		if err != nil {
			t.Errorf("parseRTMPURL(%q) error: %v", tt.raw, err)
			continue
		}
		if addr != tt.wantAddr {
			t.Errorf("parseRTMPURL(%q) addr = %q, want %q", tt.raw, addr, tt.wantAddr)
		}
		if app != tt.wantApp {
			t.Errorf("parseRTMPURL(%q) app = %q, want %q", tt.raw, app, tt.wantApp)
		}
		if key != tt.wantStreamKey {
			t.Errorf("parseRTMPURL(%q) streamKey = %q, want %q", tt.raw, key, tt.wantStreamKey)
		}
	}
}

func TestAMFWriterNumber(t *testing.T) {
	w := newAMFWriter()
	w.writeNumber(3.14)
	b := w.bytes()
	if len(b) != 9 || b[0] != amfNumber {
		t.Fatalf("bad AMF number header")
	}
}

func TestAMFWriterString(t *testing.T) {
	w := newAMFWriter()
	w.writeString("hello")
	b := w.bytes()
	if len(b) != 1+2+5 || b[0] != amfString {
		t.Fatalf("bad AMF string header")
	}
}

func TestAMFWriterObject(t *testing.T) {
	w := newAMFWriter()
	w.writeObjectStart()
	w.writeObjectEntry("app", "live")
	w.writeObjectEnd()
	b := w.bytes()
	if b[0] != amfObject {
		t.Fatalf("bad AMF object start")
	}
	// Should end with 0x000009
	if len(b) < 3 || b[len(b)-3] != 0 || b[len(b)-2] != 0 || b[len(b)-1] != 0x09 {
		t.Fatalf("bad AMF object end")
	}
}

func TestAMFReaderString(t *testing.T) {
	w := newAMFWriter()
	w.writeString("test")
	r := newAMFReader(w.bytes())
	s, err := r.readString()
	if err != nil {
		t.Fatalf("readString error: %v", err)
	}
	if s != "test" {
		t.Fatalf("readString = %q, want %q", s, "test")
	}
}

func TestAMFReaderNumber(t *testing.T) {
	w := newAMFWriter()
	w.writeNumber(42.5)
	r := newAMFReader(w.bytes())
	v, err := r.readNumber()
	if err != nil {
		t.Fatalf("readNumber error: %v", err)
	}
	if v != 42.5 {
		t.Fatalf("readNumber = %v, want %v", v, 42.5)
	}
}

func TestAVCSequenceHeader(t *testing.T) {
	// Minimal valid SPS/PPS (just need 4+ bytes SPS, 1+ byte PPS)
	sps := []byte{0x67, 0x42, 0x00, 0x1e, 0x99, 0xa0, 0x0f, 0x08}
	pps := []byte{0x68, 0xce, 0x3c, 0x80}

	avcc, err := makeAVCSequenceHeader(sps, pps)
	if err != nil {
		t.Fatalf("makeAVCSequenceHeader error: %v", err)
	}
	if len(avcc) < 11 {
		t.Fatalf("avcc too short: %d", len(avcc))
	}
	if avcc[0] != 0x01 {
		t.Fatalf("bad version: 0x%02x", avcc[0])
	}
	if avcc[1] != sps[1] {
		t.Fatalf("bad profile: 0x%02x", avcc[1])
	}
	// numSPS marker
	if avcc[5] != 0xE1 {
		t.Fatalf("bad numSPS marker: 0x%02x", avcc[5])
	}
}

func TestMakeAVCNALU(t *testing.T) {
	nalus := [][]byte{
		{0x01, 0x02, 0x03, 0x04},
		{0x05, 0x06, 0x07},
	}
	body := makeAVCNALU(frameTypeKeyFrame, nalus)
	if len(body) < 5 {
		t.Fatalf("body too short: %d", len(body))
	}
	// Frame type + codec ID: keyframe (0x10) | AVC (7) = 0x17
	if body[0] != 0x17 {
		t.Fatalf("bad frame type byte: 0x%02x, want 0x17", body[0])
	}
	if body[1] != avcNALU {
		t.Fatalf("bad AVC packet type: 0x%02x, want 1", body[1])
	}
	// Verify length-prefixed NALUs
	// First NALU: 4 bytes length prefix = 4, then data
	offset := 5
	expectedLen1 := uint32(len(nalus[0]))
	gotLen1 := uint32(body[offset])<<24 | uint32(body[offset+1])<<16 | uint32(body[offset+2])<<8 | uint32(body[offset+3])
	if gotLen1 != expectedLen1 {
		t.Fatalf("first NALU length: %d, want %d", gotLen1, expectedLen1)
	}
}

func TestMakeFLVVideoTag(t *testing.T) {
	body := []byte{0x17, 0x01, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03}
	tag := makeFLVVideoTag(1000, body)
	if len(tag) != 11+len(body) {
		t.Fatalf("tag length: %d, want %d", len(tag), 11+len(body))
	}
	if tag[0] != flvTagVideo {
		t.Fatalf("bad tag type: 0x%02x", tag[0])
	}
	// Data size in header
	size := uint32(tag[1])<<16 | uint32(tag[2])<<8 | uint32(tag[3])
	if size != uint32(len(body)) {
		t.Fatalf("data size: %d, want %d", size, len(body))
	}
}

func TestParseCommand(t *testing.T) {
	w := newAMFWriter()
	w.writeString("_result")
	w.writeNumber(1.0)
	w.writeNull()

	cmd, txn, rest, err := parseCommand(w.bytes())
	if err != nil {
		t.Fatalf("parseCommand error: %v", err)
	}
	if cmd != "_result" {
		t.Fatalf("cmd = %q, want %q", cmd, "_result")
	}
	if txn != 1.0 {
		t.Fatalf("txn = %v, want 1.0", txn)
	}
	if len(rest) == 0 {
		t.Fatal("expected non-empty rest")
	}
}

func TestMakeChunkHeader(t *testing.T) {
	// Type 0 header for cs_id=3, timestamp=1000, length=50, type=0x14, stream_id=0
	hdr := makeChunkHeader(3, 0, 1000, 50, 0x14, 0)
	if len(hdr) < 12 { // 1 byte basic + 11 bytes msg header
		t.Fatalf("type0 header too short: %d", len(hdr))
	}
	// Basic header: fmt=0, cs_id=3
	if hdr[0] != 0x03 {
		t.Fatalf("basic header: 0x%02x, want 0x03", hdr[0])
	}
	// Timestamp: 1000 = 0x0003E8
	ts := uint32(hdr[1])<<16 | uint32(hdr[2])<<8 | uint32(hdr[3])
	if ts != 1000 {
		t.Fatalf("timestamp = %d, want 1000", ts)
	}
	// Message length: 50
	ml := int(hdr[4])<<16 | int(hdr[5])<<8 | int(hdr[6])
	if ml != 50 {
		t.Fatalf("msg length = %d, want 50", ml)
	}
	// Message type
	if hdr[7] != 0x14 {
		t.Fatalf("msg type = 0x%02x, want 0x14", hdr[7])
	}

	// Type 3 header (should be just basic header)
	hdr3 := makeChunkHeader(3, 3, 0, 0, 0, 0)
	if len(hdr3) != 1 {
		t.Fatalf("type3 header length: %d, want 1", len(hdr3))
	}
	if hdr3[0] != 0xC3 {
		t.Fatalf("type3 basic header: 0x%02x, want 0xC3", hdr3[0])
	}
}
