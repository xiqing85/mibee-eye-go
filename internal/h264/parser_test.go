package h264

import (
	"bytes"
	"testing"
)

// buildAnnexB builds an Annex-B bytestream from NALU payloads with 4-byte start codes.
func buildAnnexB4(payloads ...[]byte) []byte {
	var buf bytes.Buffer
	for _, p := range payloads {
		buf.Write([]byte{0x00, 0x00, 0x00, 0x01})
		buf.Write(p)
	}
	return buf.Bytes()
}

// buildAnnexB3 builds an Annex-B bytestream from NALU payloads with 3-byte start codes.
func buildAnnexB3(payloads ...[]byte) []byte {
	var buf bytes.Buffer
	for _, p := range payloads {
		buf.Write([]byte{0x00, 0x00, 0x01})
		buf.Write(p)
	}
	return buf.Bytes()
}

func TestParseEmpty(t *testing.T) {
	p := NewParser()
	nalus := p.Parse(nil)
	if len(nalus) != 0 {
		t.Fatalf("expected 0 NALUs, got %d", len(nalus))
	}
	nalus = p.Parse([]byte{})
	if len(nalus) != 0 {
		t.Fatalf("expected 0 NALUs, got %d", len(nalus))
	}
}

func TestParseNoStartCodes(t *testing.T) {
	p := NewParser()
	nalus := p.Parse([]byte{0xFF, 0xFE, 0xFD})
	if len(nalus) != 0 {
		t.Fatalf("expected 0 NALUs for data without start codes, got %d", len(nalus))
	}
}

func TestParseSingleNALU(t *testing.T) {
	p := NewParser()
	// SPS NALU: type = 0x67 & 0x1F = 7
	naluPayload := []byte{0x67, 0x42, 0x00, 0x0A, 0xE9, 0x40, 0x40, 0x04, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x7B, 0xAC, 0x09}
	data := buildAnnexB4(naluPayload)

	nalus := p.Parse(data)
	if len(nalus) != 1 {
		t.Fatalf("expected 1 NALU, got %d", len(nalus))
	}

	n := nalus[0]
	if n.Type != 7 {
		t.Errorf("expected type 7 (SPS), got %d", n.Type)
	}
	if !n.IsSPS {
		t.Error("expected IsSPS=true")
	}
	if !bytes.Equal(n.Data, naluPayload) {
		t.Errorf("data mismatch: got %x, want %x", n.Data, naluPayload)
	}
}

func TestParseMultipleNALUs(t *testing.T) {
	p := NewParser()

	// SPS (type 7): 0x67
	sps := []byte{0x67, 0x42, 0x00, 0x0A, 0xE9, 0x40}
	// PPS (type 8): 0x68
	pps := []byte{0x68, 0xCE, 0x38, 0x80}
	// IDR (type 5): 0x65
	idr := []byte{0x65, 0xB8, 0x00, 0x04, 0x00, 0x00, 0x05, 0xEF}
	// non-IDR (type 1): 0x41
	slice := []byte{0x41, 0x9A, 0x24, 0x6C}

	data := buildAnnexB4(sps, pps, idr, slice)
	nalus := p.Parse(data)

	if len(nalus) != 4 {
		t.Fatalf("expected 4 NALUs, got %d", len(nalus))
	}

	// Verify types and flags.
	type wantNALU struct {
		typ   byte
		isSPS bool
		isPPS bool
		isIDR bool
	}
	want := []wantNALU{
		{7, true, false, false}, // SPS
		{8, false, true, false}, // PPS
		{5, false, false, true},  // IDR
		{1, false, false, false}, // non-IDR
	}

	for i, n := range nalus {
		if n.Type != want[i].typ {
			t.Errorf("NALU[%d]: type=%d, want %d", i, n.Type, want[i].typ)
		}
		if n.IsSPS != want[i].isSPS {
			t.Errorf("NALU[%d]: IsSPS=%v, want %v", i, n.IsSPS, want[i].isSPS)
		}
		if n.IsPPS != want[i].isPPS {
			t.Errorf("NALU[%d]: IsPPS=%v, want %v", i, n.IsPPS, want[i].isPPS)
		}
		if n.IsIDR != want[i].isIDR {
			t.Errorf("NALU[%d]: IsIDR=%v, want %v", i, n.IsIDR, want[i].isIDR)
		}
	}
}

func TestParseStartCodeVariants(t *testing.T) {
	p := NewParser()

	// Test 3-byte start codes.
	sps := []byte{0x67, 0x42, 0x00, 0x0A}
	pps := []byte{0x68, 0xCE, 0x38}
	data3 := buildAnnexB3(sps, pps)
	nalus3 := p.Parse(data3)
	if len(nalus3) != 2 {
		t.Fatalf("3-byte start codes: expected 2 NALUs, got %d", len(nalus3))
	}
	if nalus3[0].Type != 7 || !nalus3[0].IsSPS {
		t.Error("3-byte: first NALU should be SPS")
	}
	if nalus3[1].Type != 8 || !nalus3[1].IsPPS {
		t.Error("3-byte: second NALU should be PPS")
	}

	// Test 4-byte start codes.
	data4 := buildAnnexB4(sps, pps)
	nalus4 := p.Parse(data4)
	if len(nalus4) != 2 {
		t.Fatalf("4-byte start codes: expected 2 NALUs, got %d", len(nalus4))
	}
	if nalus4[0].Type != 7 || !nalus4[0].IsSPS {
		t.Error("4-byte: first NALU should be SPS")
	}

	// Test mixed 3-byte and 4-byte start codes.
	var mixed bytes.Buffer
	mixed.Write([]byte{0x00, 0x00, 0x00, 0x01}) // 4-byte
	mixed.Write(sps)
	mixed.Write([]byte{0x00, 0x00, 0x01}) // 3-byte
	mixed.Write(pps)

	nalusMixed := p.Parse(mixed.Bytes())
	if len(nalusMixed) != 2 {
		t.Fatalf("mixed start codes: expected 2 NALUs, got %d", len(nalusMixed))
	}
	if !bytes.Equal(nalusMixed[0].Data, sps) {
		t.Error("mixed: first NALU data mismatch")
	}
	if !bytes.Equal(nalusMixed[1].Data, pps) {
		t.Error("mixed: second NALU data mismatch")
	}
}

func TestExtractSPSandPPS(t *testing.T) {
	p := NewParser()

	// Without SPS/PPS.
	nalus := []NALU{{Type: 1, Data: []byte{0x41}}}
	sps, pps, found := p.ExtractSPSandPPS(nalus)
	if found {
		t.Error("should not find SPS+PPS")
	}
	if sps != nil || pps != nil {
		t.Error("sps/pps should be nil")
	}

	// With SPS only.
	nalus = []NALU{
		{Type: 7, Data: []byte{0x67, 0x42}, IsSPS: true},
		{Type: 1, Data: []byte{0x41}},
	}
	sps, pps, found = p.ExtractSPSandPPS(nalus)
	if found {
		t.Error("should not find SPS+PPS with only SPS")
	}
	if sps == nil {
		t.Error("sps should not be nil")
	}

	// With both SPS and PPS.
	nalus = []NALU{
		{Type: 1, Data: []byte{0x41}},
		{Type: 7, Data: []byte{0x67, 0x42}, IsSPS: true},
		{Type: 8, Data: []byte{0x68, 0xCE}, IsPPS: true},
		{Type: 5, Data: []byte{0x65}, IsIDR: true},
	}
	sps, pps, found = p.ExtractSPSandPPS(nalus)
	if !found {
		t.Error("should find SPS+PPS")
	}
	if !bytes.Equal(sps, []byte{0x67, 0x42}) {
		t.Errorf("sps mismatch: %x", sps)
	}
	if !bytes.Equal(pps, []byte{0x68, 0xCE}) {
		t.Errorf("pps mismatch: %x", pps)
	}
}

func TestIDRDetection(t *testing.T) {
	p := NewParser()

	// IDR slice: type 5 (0x65 & 0x1F = 5)
	idr := []byte{0x65, 0x88, 0x84, 0x00, 0x40, 0xFF, 0xFE, 0xF8, 0xC0}
	data := buildAnnexB4(idr)

	nalus := p.Parse(data)
	if len(nalus) != 1 {
		t.Fatalf("expected 1 NALU, got %d", len(nalus))
	}

	n := nalus[0]
	if n.Type != 5 {
		t.Errorf("expected type 5, got %d", n.Type)
	}
	if !n.IsIDR {
		t.Error("expected IsIDR=true")
	}
	if n.IsSPS || n.IsPPS {
		t.Error("IDR should not be SPS or PPS")
	}
}

func TestFindStartCodes(t *testing.T) {
	p := NewParser()

	tests := []struct {
		name string
		data []byte
		want []int
	}{
		{
			name: "no start codes",
			data: []byte{0x01, 0x02, 0x03},
			want: nil,
		},
		{
			name: "single 4-byte start code",
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x67},
			want: []int{0},
		},
		{
			name: "single 3-byte start code",
			data: []byte{0x00, 0x00, 0x01, 0x67},
			want: []int{0},
		},
		{
			name: "two start codes",
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x00, 0x00, 0x00, 0x01, 0x65},
			want: []int{0, 5},
		},
		{
			name: "mixed 3 and 4 byte start codes",
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x00, 0x00, 0x01, 0x65},
			want: []int{0, 5},
		},
		{
			name: "too short",
			data: []byte{0x00, 0x01},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.FindStartCodes(tt.data)
			if !slicesEqual(got, tt.want) {
				t.Errorf("FindStartCodes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func slicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
