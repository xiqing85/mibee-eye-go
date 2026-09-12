// Minimal MPEG-TS muxer for H.264 video.
//
// Produces valid MPEG-TS segments suitable for HLS live streaming.
// PAT (PID 0x0000), PMT (PID 0x1000), video PES (PID 0x0100).
// No audio, no DRM, no fMP4 — MPEG-TS only.
//
// Reference: ISO/IEC 13818-1 (MPEG-2 Systems).
package hls

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
)

const (
	tsPacketSize   = 188
	tsSyncByte     = 0x47
	pidPAT         = 0x0000
	pidPMT         = 0x1000
	pidVideo       = 0x0100
	streamTypeH264 = 0x1B
	streamIDVideo  = 0xE0
	programNumber  = 1
)

// segmentBuilder builds a single MPEG-TS segment from H.264 access units.
// Each segment starts with PAT/PMT tables followed by PES packets.
type segmentBuilder struct {
	buf bytes.Buffer
	cc  [3]uint8 // continuity counters: [0]=PAT, [1]=PMT, [2]=video
}

const (
	ccPAT   = 0
	ccPMT   = 1
	ccVideo = 2
)

// newSegment initialises a new segment and writes PAT/PMT.
// sps and pps are stored for prepending to the first IDR in this segment.
func newSegment() *segmentBuilder {
	sb := &segmentBuilder{}
	sb.writePAT()
	sb.writePMT()
	return sb
}

// writeAU appends an access unit as one or more PES packets.
// The NALUs should be provided without start codes (raw NAL unit data).
// If sps/pps are non-nil and this is a keyframe, they are prepended.
// If sps/pps provided and au is keyframe, sps+pps are prepended first, then idr.
// sps and pps are raw NALU data without start codes.
func (sb *segmentBuilder) writeAU(nalus [][]byte, pts int64, keyframe bool, sps, pps []byte) {
	// Build the H.264 access unit data in Annex-B format.
	var auData bytes.Buffer
	startCode := []byte{0x00, 0x00, 0x00, 0x01}

	// For keyframes, prepend SPS and PPS if available.
	if keyframe {
		if len(sps) > 0 {
			auData.Write(startCode)
			auData.Write(sps)
		}
		if len(pps) > 0 {
			auData.Write(startCode)
			auData.Write(pps)
		}
	}

	// Write all NALUs with start codes.
	for _, nalu := range nalus {
		auData.Write(startCode)
		auData.Write(nalu)
	}

	pesPayload := auData.Bytes()

	// Build PES packet.
	// PES header: [start_code_prefix 0x000001][stream_id][packet_length][optional_header][data]
	pesHeader := buildPESHeader(pts, len(pesPayload))

	// Write the complete PES data as TS packets.
	totalPES := make([]byte, 0, len(pesHeader)+len(pesPayload))
	totalPES = append(totalPES, pesHeader...)
	totalPES = append(totalPES, pesPayload...)
	sb.writePackets(pidVideo, true, totalPES, &sb.cc[ccVideo])
}

// writePAT writes the Program Association Table as TS packets.
func (sb *segmentBuilder) writePAT() {
	// Build the PAT section data (excluding TS headers).
	// Table 2-30 of ISO/IEC 13818-1.
	patBytes := []byte{
		// table_id: 0x00 (PAT)
		0x00,
		// section_syntax_indicator(1) | '0'(1) | reserved(2) | section_length(12)
		// section_length = 13 = bytes from transport_stream_id to CRC inclusive
		0xB0, 0x0D,
		// transport_stream_id = 1
		0x00, 0x01,
		// reserved(2) | version_number(5) | current_next_indicator(1) = 11 00000 1
		0xC1,
		// section_number = 0
		0x00,
		// last_section_number = 0
		0x00,
		// program_number = 1
		0x00, 0x01,
		// reserved(3) | program_map_PID(13) = 111 10000 00000000 = PMT PID 0x1000
		0xF0, 0x00,
	}

	// Append CRC32.
	crc := crc32.ChecksumIEEE(patBytes)
	crcBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(crcBytes, crc)
	patBytes = append(patBytes, crcBytes...)

	// Write as TS packets with pointer field (PSI section).
	payload := make([]byte, 1+len(patBytes))
	payload[0] = 0 // pointer_field = 0 (section starts at first byte)
	copy(payload[1:], patBytes)
	sb.writePackets(pidPAT, true, payload, &sb.cc[ccPAT])
}

// writePMT writes the Program Map Table as TS packets.
func (sb *segmentBuilder) writePMT() {
	// Build the PMT section data (excluding TS headers).
	pmtBytes := []byte{
		// table_id: 0x02 (PMT)
		0x02,
		// section_syntax_indicator(1) | '0'(1) | reserved(2) | section_length(12)
		// section_length = 18
		0xB0, 0x12,
		// program_number = 1
		0x00, 0x01,
		// reserved(2) | version_number(5) | current_next_indicator(1) = 11 00000 1
		0xC1,
		// section_number = 0
		0x00,
		// last_section_number = 0
		0x00,
		// reserved(3) | PCR_PID(13) = 111 00001 00000000 = video PID 0x0100
		0xE1, 0x00,
		// reserved(4) | program_info_length(12) = 1111 000000000000 = 0
		0xF0, 0x00,
		// --- stream loop (one H.264 video stream) ---
		// stream_type = 0x1B (H.264)
		0x1B,
		// reserved(3) | elementary_PID(13) = 111 00001 00000000 = video PID 0x0100
		0xE1, 0x00,
		// reserved(4) | ES_info_length(12) = 1111 000000000000 = 0
		0xF0, 0x00,
	}

	// Append CRC32.
	crc := crc32.ChecksumIEEE(pmtBytes)
	crcBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(crcBytes, crc)
	pmtBytes = append(pmtBytes, crcBytes...)

	// Write as TS packets with pointer field (PSI section).
	payload := make([]byte, 1+len(pmtBytes))
	payload[0] = 0 // pointer_field = 0
	copy(payload[1:], pmtBytes)
	sb.writePackets(pidPMT, true, payload, &sb.cc[ccPMT])
}

// writePackets writes raw payload data across one or more TS packets.
// PUSI is set only on the first packet.
// For PSI sections, the payload must already contain the pointer_field.
// For PES data, no pointer_field is needed — the data starts directly.
func (sb *segmentBuilder) writePackets(pid uint16, pusFirst bool, data []byte, cc *uint8) {
	pos := 0
	first := true
	for pos < len(data) {
		var pkt [tsPacketSize]byte
		pkt[0] = tsSyncByte
		pkt[1] = byte((pid >> 8) & 0x1F)
		pkt[2] = byte(pid & 0xFF)
		if first && pusFirst {
			pkt[1] |= 0x40 // PUSI bit
		}
		// Adaptation field control: 01 = payload only (no adaptation)
		pkt[3] = 0x10 | (*cc & 0x0F)

		chunk := tsPacketSize - 4
		if pos+chunk > len(data) {
			chunk = len(data) - pos
		}
		copy(pkt[4:], data[pos:pos+chunk])

		// Write the completed TS packet.
		sb.buf.Write(pkt[:])

		pos += chunk
		*cc = (*cc + 1) & 0x0F
		first = false
	}
}

// bytes returns the complete MPEG-TS segment data.
func (sb *segmentBuilder) bytes() []byte {
	return sb.buf.Bytes()
}

// buildPESHeader constructs the PES packet header for H.264 video.
// For video, PES_packet_length is set to 0 (unbounded).
// pts is in 90kHz clock units.
func buildPESHeader(pts int64, _ int) []byte {
	// PES header fields:
	// 3 bytes: packet_start_code_prefix (0x00 0x00 0x01)
	// 1 byte:  stream_id (0xE0 for video)
	// 2 bytes: PES_packet_length (0 = unbounded)
	// 2 bytes: optional PES header flags
	// 1 byte:  PES header data length
	// 5 bytes: PTS

	hdr := make([]byte, 14)
	hdr[0] = 0x00
	hdr[1] = 0x00
	hdr[2] = 0x01
	hdr[3] = streamIDVideo // 0xE0
	// PES_packet_length = 0 (unbounded for video)
	hdr[4] = 0x00
	hdr[5] = 0x00
	// Optional PES header flags:
	// bit 15-14: '10' marker bits
	// bit 13:    0 (no scrambling)
	// bit 12:    0 (not prioritised)
	// bit 11:    1 (data alignment indicator)
	// bit 10:    0 (no copyright)
	// bit 9:     0 (original)
	// bit 8-7:   '10' = PTS only
	// bit 6-0:   0
	hdr[6] = 0x84 // 1000 0100
	hdr[7] = 0x00
	// PES header data length = 5 (PTS only)
	hdr[8] = 0x05

	// Encode PTS (33-bit, 90kHz clock, with marker bits per Table 2-7).
	// PTS is encoded in 5 bytes with marker bits at bit 0 of bytes 0, 2, 4.
	// byte 0: '0010'(4) | PTS[32:30](3) | marker=1(1)
	// byte 1: PTS[29:22](8)
	// byte 2: PTS[21:15](7) | marker=1(1)
	// byte 3: PTS[14:7](8)
	// byte 4: PTS[6:0](7) | marker=1(1)
	hdr[9] = 0x21 | byte((pts>>30)&0x0E) // 0010 P[32:30] 1
	hdr[10] = byte(pts >> 22)             // P[29:22]
	hdr[11] = byte((pts>>15)&0xFE) | 0x01 // P[21:15] 1
	hdr[12] = byte(pts >> 7)              // P[14:7]
	hdr[13] = byte(pts<<1) | 0x01         // P[6:0] 1

	return hdr
}
