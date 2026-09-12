// FLV tag muxing for H.264 video.
//
// Converts H.264 Annex-B NALUs into FLV video tags suitable for
// RTMP streaming. Produces AVC sequence headers (AVCC extradata)
// from SPS/PPS and AVC NALUs in length-prefixed format.
package rtmp

import (
	"encoding/binary"
	"fmt"
)

// FLV tag types.
const (
	flvTagAudio = 0x08
	flvTagVideo = 0x09
	flvTagMeta  = 0x12
)

// Frame types (upper 4 bits of first byte in FLV video tag body).
const (
	frameTypeKeyFrame    = 1 << 4 // 0x10
	frameTypeInterFrame  = 2 << 4 // 0x20
	frameTypeDisposable  = 3 << 4 // 0x30
)

// Codec IDs (lower 4 bits of first byte in FLV video tag body).
const (
	codecIDAVC = 7 // H.264
)

// AVC packet types.
const (
	avcSeqHeader = 0 // AVC sequence header (AVCC extradata)
	avcNALU      = 1 // AVC NALU
	avcEndOfSeq  = 2 // AVC end of sequence (optional)
)

// makeFLVVideoTag creates a complete FLV video tag (tag header + tag body).
//
// Tag header (11 bytes):
//
//	[1 byte tag type] [3 bytes data size] [3 bytes timestamp] [1 byte timestamp ext] [3 bytes stream ID]
//
// The stream ID is always 0. The timestamp is in milliseconds.
// Returns the complete tag including the 11-byte header.
func makeFLVVideoTag(timestamp uint32, body []byte) []byte {
	size := uint32(len(body))
	tag := make([]byte, 11+size)

	// Tag header
	tag[0] = flvTagVideo
	tag[1] = byte(size >> 16) // data size (UI24 big-endian)
	tag[2] = byte(size >> 8)
	tag[3] = byte(size)
	tag[4] = byte(timestamp >> 16) // timestamp (UI24 big-endian)
	tag[5] = byte(timestamp >> 8)
	tag[6] = byte(timestamp)
	tag[7] = byte(timestamp >> 24) // timestamp extension (UI8, MSB of 32-bit timestamp)
	tag[8] = 0                     // stream ID (UI24, always 0)
	tag[9] = 0
	tag[10] = 0

	// Tag body
	copy(tag[11:], body)
	return tag
}

// makeAVCSequenceHeader builds an AVC sequence header (AVCC extradata)
// from SPS and PPS NALU data (raw NALU bytes WITHOUT start codes or length prefixes).
//
// AVCC extradata format:
//
//	[1B version=0x01] [1B profile] [1B compatibility] [1B level] [1B lengthSize=0xFF]
//	[1B numSPS (top 3 bits reserved)] [2B sps length] [SPS data]
//	[1B numPPS] [2B pps length] [PPS data]
func makeAVCSequenceHeader(sps, pps []byte) ([]byte, error) {
	if len(sps) < 4 {
		return nil, fmt.Errorf("flv: SPS too short (%d bytes)", len(sps))
	}
	if len(pps) < 1 {
		return nil, fmt.Errorf("flv: PPS too short (%d bytes)", len(pps))
	}

	// Header: 5 bytes
	body := make([]byte, 0, 16+len(sps)+len(pps))
	body = append(body, 0x01)                   // version
	body = append(body, sps[1])                  // profile (AVCProfileIndication)
	body = append(body, sps[2])                  // compatibility (profile_compatibility)
	body = append(body, sps[3])                  // level (AVCLevelIndication)
	body = append(body, 0xFF)                    // lengthSizeMinusOne (top 6 bits 1, bottom 2 = 0x11 for 4-byte)
	// Actually: 0xFC | 0x03 = 0xFF for 4-byte NALU lengths
	// NalUnitLengthSize = (reserved(1) << 6 | 0x3F) = 0xFF for 4 bytes

	// SPS
	body = append(body, 0xE1) // reserved(3) | numSPS(5) = 0xE0 | 1 = 0xE1

	var tmp [2]byte
	binary.BigEndian.PutUint16(tmp[:], uint16(len(sps)))
	body = append(body, tmp[:]...)
	body = append(body, sps...)

	// PPS
	body = append(body, 0x01) // numPPS = 1
	binary.BigEndian.PutUint16(tmp[:], uint16(len(pps)))
	body = append(body, tmp[:]...)
	body = append(body, pps...)

	return body, nil
}

// makeAVCNALU builds the body of an FLV video tag for AVC NALU data.
//
// Body format:
//
//	[1B frameType|codecID] [1B avcPacketType=1] [3B compositionTime=0]
//	[NALU data in AVCC format: 4-byte length prefixes instead of start codes]
//
// The rawNalus parameter is a list of NALU data bytes (without start codes).
// They are converted to length-prefixed format (4 bytes BE length + NALU data).
func makeAVCNALU(frameType byte, rawNalus [][]byte) []byte {
	// Calculate total AVCC payload size
	totalLen := 0
	for _, nalu := range rawNalus {
		totalLen += 4 + len(nalu) // 4-byte length prefix + NALU data
	}

	// Body: 5 bytes header + AVCC payload
	body := make([]byte, 5+totalLen)
	body[0] = frameType | codecIDAVC // frame type + codec ID
	body[1] = avcNALU                // AVC packet type
	body[2] = 0                      // composition time (3 bytes, 0 for H.264 without B-frames)
	body[3] = 0
	body[4] = 0

	offset := 5
	for _, nalu := range rawNalus {
		binary.BigEndian.PutUint32(body[offset:], uint32(len(nalu)))
		offset += 4
		copy(body[offset:], nalu)
		offset += len(nalu)
	}

	return body
}

// annexBToAVCC converts raw H.264 Annex-B bytestream (with start codes)
// to AVCC format (4-byte length prefixes).
// Returns a slice of NALU data pieces (without length prefixes).
func annexBToAVCC(data []byte) ([][]byte, error) {
	var nalus [][]byte
	start := 0
	i := 0

	for i < len(data)-3 {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			// Three-byte start code at i
			if i > start {
				nalus = append(nalus, data[start:i])
			}
			i += 3
			start = i
			// Check for 4-byte start code: if data[i-1] was also 0, we already have 0x000001
			// and the preceding 0 is part of a 4-byte start code.
			// Four-byte start code: 0x00 0x00 0x00 0x01
			if i >= start { // already moved past 3 bytes, check if it was a 4-byte
				// Actually, if we found 0x000001 at position i (current), and data[i-1] == 0,
				// then it's a 4-byte start code starting at i-1.
				// But since we started our check at i, we already handle this correctly.
				// The 4-byte case: data[i-4] == 0, and we found 0x000001 at i-1
				// Hmm, let me reconsider.
				//
				// The loop checks each position. If we detect 0x000001 at position i (3-byte SC),
				// and the byte before (i-1) is 0, then it's actually a 4-byte SC at i-1.
				continue
			}
		} else if i < len(data)-4 && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			// Four-byte start code at i
			if i > start {
				nalus = append(nalus, data[start:i])
			}
			i += 4
			start = i
			continue
		}
		i++
	}

	// Remaining data
	if start < len(data) {
		nalus = append(nalus, data[start:])
	}

	return nalus, nil
}

// h264AVCCBody produces the FLV video tag body for a set of H.264 NALUs.
// If an SPS or PPS NALU is present and sps/pps are provided, a sequence header
// is returned instead of a NALU body. The function decides which to produce:
// a keyframe always emits a sequence header first if sps/pps are available.
//
// Returns (isSeqHeader bool, body []byte, error).
func h264AVCCBody(keyframe bool, rawNalus [][]byte, sps, pps []byte) (bool, []byte, error) {
	if keyframe && len(sps) > 0 && len(pps) > 0 {
		seqHeader, err := makeAVCSequenceHeader(sps, pps)
		if err != nil {
			return false, nil, fmt.Errorf("flv: sequence header: %w", err)
		}
		body := makeAVCNALU(frameTypeKeyFrame, [][]byte{seqHeader})
		body[1] = avcSeqHeader // override packet type to 0
		return true, body, nil
	}

	var frameType byte = frameTypeInterFrame
	if keyframe {
		frameType = frameTypeKeyFrame
	}
	body := makeAVCNALU(frameType, rawNalus)
	return false, body, nil
}
