package h264

// NALU represents a single H.264 NAL Unit.
type NALU struct {
	Type  byte   // NALU type (first byte & 0x1F)
	Data  []byte // Raw NALU data (without start code)
	IsIDR bool   // True if type == 5
	IsSPS bool   // True if type == 7
	IsPPS bool   // True if type == 8
}

// Parser parses H.264 Annex-B bytestreams into NALUs.
type Parser struct{}

// NewParser creates a new H.264 Annex-B parser.
func NewParser() *Parser {
	return &Parser{}
}

// Parse splits Annex-B data into individual NALUs.
// Start codes: 0x00000001 (4-byte) or 0x000001 (3-byte).
// NALU type: first byte after start code, type = byte & 0x1F.
// Types: 1=non-IDR, 5=IDR, 6=SEI, 7=SPS, 8=PPS.
func (p *Parser) Parse(data []byte) []NALU {
	if len(data) == 0 {
		return nil
	}

	positions := p.FindStartCodes(data)
	if len(positions) == 0 {
		return nil
	}

	nalus := make([]NALU, 0, len(positions))

	for i, pos := range positions {
		// Determine the NALU data start (skip start code).
		var naluStart int
		if pos+4 <= len(data) && data[pos] == 0 && data[pos+1] == 0 && data[pos+2] == 0 && data[pos+3] == 1 {
			naluStart = pos + 4
		} else {
			naluStart = pos + 3
		}

		if naluStart >= len(data) {
			break
		}

		// Find the end: next start code or end of data.
		var naluEnd int
		if i+1 < len(positions) {
			naluEnd = positions[i+1]
		} else {
			naluEnd = len(data)
		}

		naluData := data[naluStart:naluEnd]
		if len(naluData) == 0 {
			continue
		}

		naluType := naluData[0] & 0x1F

		nalus = append(nalus, NALU{
			Type:  naluType,
			Data:  naluData,
			IsIDR: naluType == 5,
			IsSPS: naluType == 7,
			IsPPS: naluType == 8,
		})
	}

	return nalus
}

// FindStartCodes returns indices of all start code positions in data.
// Matches both 4-byte (0x00000001) and 3-byte (0x000001) start codes.
func (p *Parser) FindStartCodes(data []byte) []int {
	if len(data) < 3 {
		return nil
	}

	var positions []int
	i := 0

	for i < len(data)-2 {
		// Look for 0x000001 pattern (the core of both 3-byte and 4-byte start codes).
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			// Check if preceded by 0x00 → 4-byte start code at i-1.
			if i > 0 && data[i-1] == 0 {
				positions = append(positions, i-1)
			} else {
				positions = append(positions, i)
			}
			i += 3
			continue
		}
		i++
	}

	return positions
}

// ExtractSPSandPPS scans NALUs and returns SPS and PPS data if found.
func (p *Parser) ExtractSPSandPPS(nalus []NALU) (sps []byte, pps []byte, found bool) {
	for _, nalu := range nalus {
		if nalu.IsSPS && sps == nil {
			sps = nalu.Data
		}
		if nalu.IsPPS && pps == nil {
			pps = nalu.Data
		}
	}
	return sps, pps, sps != nil && pps != nil
}
