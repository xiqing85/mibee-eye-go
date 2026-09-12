package camera

import (
	"bytes"
	"testing"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/h264"
)

// buildAnnexB joins NALUs into an Annex-B bytestream with 4-byte start codes.
func buildAnnexB(nalus ...[]byte) []byte {
	var out []byte
	for _, n := range nalus {
		out = append(out, 0x00, 0x00, 0x00, 0x01)
		out = append(out, n...)
	}
	return out
}

func TestExtractCompleteNALUs(t *testing.T) {
	parser := h264.NewParser()
	stream := buildAnnexB(
		[]byte{0x67, 0x42, 0x00}, // SPS
		[]byte{0x68, 0xCE},       // PPS
		[]byte{0x65, 0x88},       // IDR
		[]byte{0x41, 0x9A},       // slice
	)

	// The last NALU has no following start code to delimit it, so it is
	// held back as a potentially-partial trailing NALU.
	nalus, rest := extractCompleteNALUs(stream, parser)
	if len(nalus) != 3 {
		t.Fatalf("expected 3 complete NALUs, got %d", len(nalus))
	}
	if len(rest) == 0 {
		t.Fatal("expected trailing partial NALU to be kept")
	}

	types := []byte{7, 8, 5}
	for i, n := range nalus {
		if n.Type != types[i] {
			t.Errorf("NALU[%d] type = %d, want %d", i, n.Type, types[i])
		}
	}
	if !nalus[0].IsSPS || !nalus[1].IsPPS || !nalus[2].IsIDR {
		t.Errorf("SPS/PPS/IDR flags not set: %+v", nalus)
	}
}

func TestExtractCompleteNALUsPartialTrailing(t *testing.T) {
	parser := h264.NewParser()
	// Stream ends mid-NALU: the last slice is truncated.
	stream := buildAnnexB(
		[]byte{0x67, 0x42, 0x00}, // SPS
		[]byte{0x41, 0x9A, 0x01}, // slice, truncated (no following start code)
	)

	nalus, rest := extractCompleteNALUs(stream, parser)
	if len(nalus) != 1 {
		t.Fatalf("expected 1 complete NALU, got %d", len(nalus))
	}
	if nalus[0].Type != 7 {
		t.Errorf("NALU type = %d, want 7 (SPS)", nalus[0].Type)
	}
	// The trailing partial NALU (start code + data) must be kept.
	if len(rest) == 0 {
		t.Fatal("expected trailing partial NALU to be kept")
	}
	if !bytes.HasPrefix(rest, []byte{0x00, 0x00, 0x00, 0x01}) {
		t.Errorf("remaining bytes should start with a start code, got %x", rest)
	}
}

func TestExtractCompleteNALUsChunked(t *testing.T) {
	parser := h264.NewParser()
	stream := buildAnnexB(
		[]byte{0x67, 0x42, 0x00}, // SPS
		[]byte{0x68, 0xCE},       // PPS
		[]byte{0x65, 0x88},       // IDR
		[]byte{0x41, 0x9A},       // slice
	)

	// Feed the stream in awkward chunks, including splits inside start codes.
	// A trailing start code delimits the final NALU (as the next read would).
	chunks := [][]byte{
		stream[0:3],
		stream[3:7],
		stream[7:9],
		stream[9:],
		{0x00, 0x00, 0x00, 0x01},
	}

	var pending []byte
	var all []h264.NALU
	for _, chunk := range chunks {
		pending = append(pending, chunk...)
		var nalus []h264.NALU
		nalus, pending = extractCompleteNALUs(pending, parser)
		all = append(all, nalus...)
	}

	if len(all) != 4 {
		t.Fatalf("expected 4 NALUs across chunks, got %d", len(all))
	}
	types := []byte{7, 8, 5, 1}
	for i, n := range all {
		if n.Type != types[i] {
			t.Errorf("NALU[%d] type = %d, want %d", i, n.Type, types[i])
		}
	}
}

func TestStartsNewAU(t *testing.T) {
	sps := h264.NALU{Type: 7, IsSPS: true}
	pps := h264.NALU{Type: 8, IsPPS: true}
	idr := h264.NALU{Type: 5, IsIDR: true}
	slice := h264.NALU{Type: 1}

	tests := []struct {
		name string
		nalu h264.NALU
		cur  []h264.NALU
		want bool
	}{
		{"SPS starts new AU", sps, nil, true},
		{"PPS joins SPS AU", pps, []h264.NALU{sps}, false},
		{"IDR joins SPS PPS AU", idr, []h264.NALU{sps, pps}, false},
		{"slice after keyframe starts new AU", slice, []h264.NALU{sps, pps, idr}, true},
		{"slice after slice starts new AU", slice, []h264.NALU{slice}, true},
		{"PPS alone starts new AU", pps, nil, true},
		{"IDR alone starts new AU", idr, nil, true},
		{"IDR joins PPS-only AU", idr, []h264.NALU{pps}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := startsNewAU(tt.nalu, tt.cur); got != tt.want {
				t.Errorf("startsNewAU(%+v, %+v) = %v, want %v", tt.nalu, tt.cur, got, tt.want)
			}
		})
	}
}

func TestGroupAccessUnits(t *testing.T) {
	// A trailing start code delimits the final slice (as the next read would).
	stream := buildAnnexB(
		[]byte{0x67, 0x42, 0x00}, // SPS
		[]byte{0x68, 0xCE},       // PPS
		[]byte{0x65, 0x88},       // IDR
		[]byte{0x41, 0x9A},       // slice 1
		[]byte{0x41, 0x9B},       // slice 2
		[]byte{0x41, 0x9C},       // slice 3
		[]byte{0x67, 0x42, 0x00}, // SPS
		[]byte{0x68, 0xCE},       // PPS
		[]byte{0x65, 0x88},       // IDR
		[]byte{0x41, 0x9D},       // slice 4
	)
	stream = append(stream, 0x00, 0x00, 0x00, 0x01)

	parser := h264.NewParser()
	nalus, _ := extractCompleteNALUs(stream, parser)
	if len(nalus) != 10 {
		t.Fatalf("expected 10 NALUs, got %d", len(nalus))
	}

	var curAU []h264.NALU
	var aus [][]h264.NALU
	for _, nalu := range nalus {
		if startsNewAU(nalu, curAU) && len(curAU) > 0 {
			aus = append(aus, curAU)
			curAU = nil
		}
		curAU = append(curAU, nalu)
	}
	if len(curAU) > 0 {
		aus = append(aus, curAU)
	}

	if len(aus) != 6 {
		t.Fatalf("expected 6 access units, got %d", len(aus))
	}

	// AU 0: keyframe SPS PPS IDR
	if len(aus[0]) != 3 || !aus[0][0].IsSPS || !aus[0][1].IsPPS || !aus[0][2].IsIDR {
		t.Errorf("AU[0] = %+v, want [SPS PPS IDR]", aus[0])
	}
	// AU 1-3: single slices
	for i := 1; i <= 3; i++ {
		if len(aus[i]) != 1 || aus[i][0].Type != 1 {
			t.Errorf("AU[%d] = %+v, want single slice", i, aus[i])
		}
	}
	// AU 4: second keyframe SPS PPS IDR
	if len(aus[4]) != 3 || !aus[4][0].IsSPS || !aus[4][1].IsPPS || !aus[4][2].IsIDR {
		t.Errorf("AU[4] = %+v, want [SPS PPS IDR]", aus[4])
	}
	// AU 5: final slice
	if len(aus[5]) != 1 || aus[5][0].Type != 1 {
		t.Errorf("AU[5] = %+v, want single slice", aus[5])
	}
}

func TestBuildArgsFlips(t *testing.T) {
	// Device-level flips from config must reach the rpicam-vid command
	// line — they are baked into the encoded stream (libcamera transform).
	base := func() *RPiCamVidCamera {
		c := NewRPiCamVidCamera(
			WithVidBinPath("rpicam-vid"),
			WithVidParams(DefaultParams()),
			WithVidInfo(CameraInfo{}),
		)
		return c
	}

	off := base()
	got := off.buildArgs()
	for _, a := range got {
		if a == "--hflip" || a == "--vflip" {
			t.Errorf("unexpected flip flag %q with flips disabled: %v", a, got)
		}
	}

	hv := base()
	hv.mu.Lock()
	hv.params.HFlip = true
	hv.params.VFlip = true
	hv.mu.Unlock()
	got = hv.buildArgs()
	hasH, hasV := false, false
	for _, a := range got {
		if a == "--hflip" {
			hasH = true
		}
		if a == "--vflip" {
			hasV = true
		}
	}
	if !hasH || !hasV {
		t.Errorf("expected --hflip and --vflip in args, got %v", got)
	}
}
