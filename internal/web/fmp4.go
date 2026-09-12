package web

// Fragmented MP4 (fMP4) muxer for MSE — server-side port of the Rust
// implementation in mibee-eye-raspi-rs (src/web/fmp4.rs). Produces an init
// segment (ftyp+moov) followed by one moof+mdat fragment per access unit,
// streamed over chunked HTTP (SPEC v1 §4.1).

import (
	"encoding/binary"
)

var fmp4Matrix = []byte{
	0x00, 0x01, 0x00, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0x00, 0x01, 0x00, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0x40, 0x00, 0x00, 0x00,
}

func u16be(buf []byte, v uint16) []byte {
	return append(buf, byte(v>>8), byte(v))
}

func u24be(buf []byte, v uint32) []byte {
	return append(buf, byte(v>>16), byte(v>>8), byte(v))
}

func u32be(buf []byte, v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return append(buf, b[:]...)
}

func u64be(buf []byte, v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return append(buf, b[:]...)
}

// box builds [size(4)][type(4)][payload].
func mp4Box(boxType string, payload []byte) []byte {
	out := make([]byte, 0, 8+len(payload))
	out = u32be(out, uint32(8+len(payload)))
	out = append(out, boxType...)
	return append(out, payload...)
}

// fullBox builds [size(4)][type(4)][version(1)][flags(3)][payload].
func mp4FullBox(boxType string, version byte, flags uint32, payload []byte) []byte {
	out := make([]byte, 0, 12+len(payload))
	out = u32be(out, uint32(12+len(payload)))
	out = append(out, boxType...)
	out = append(out, version)
	out = u24be(out, flags)
	return append(out, payload...)
}

// buildInitSegment builds the fMP4 initialization segment (ftyp + moov).
func buildInitSegment(sps, pps []byte, width, height uint32) []byte {
	// ftyp
	ftyp := []byte("iso5")
	ftyp = u32be(ftyp, 512)
	ftyp = append(ftyp, "iso5avc1mp42"...)
	ftypBox := mp4Box("ftyp", ftyp)

	// avcC
	avcc := []byte{1}                           // configurationVersion
	avcc = append(avcc, sps[1], sps[2], sps[3]) // profile/compat/level
	avcc = append(avcc, 0xFF)                   // lengthSizeMinusOne=3 | reserved
	avcc = append(avcc, 0xE1)                   // numSPS=1 | reserved
	avcc = u16be(avcc, uint16(len(sps)))
	avcc = append(avcc, sps...)
	avcc = append(avcc, 1) // numPPS
	avcc = u16be(avcc, uint16(len(pps)))
	avcc = append(avcc, pps...)
	avccBox := mp4Box("avcC", avcc)

	// avc1 sample entry
	avc1 := make([]byte, 0, 78+len(avccBox))
	avc1 = append(avc1, make([]byte, 6)...)  // reserved
	avc1 = u16be(avc1, 1)                    // data_reference_index
	avc1 = append(avc1, make([]byte, 16)...) // pre_defined + reserved
	avc1 = u16be(avc1, uint16(width))
	avc1 = u16be(avc1, uint16(height))
	avc1 = u32be(avc1, 0x00480000)           // horizresolution 72dpi
	avc1 = u32be(avc1, 0x00480000)           // vertresolution
	avc1 = u32be(avc1, 0)                    // reserved
	avc1 = u16be(avc1, 1)                    // frame_count
	avc1 = append(avc1, make([]byte, 32)...) // compressorname
	avc1 = u16be(avc1, 0x0018)               // depth=24
	avc1 = append(avc1, 0xFF, 0xFF)          // pre_defined
	avc1 = append(avc1, avccBox...)
	avc1Box := mp4Box("avc1", avc1)

	// stsd + empty sample table boxes
	stsd := u32be(nil, 1)
	stsd = append(stsd, avc1Box...)
	stsdBox := mp4FullBox("stsd", 0, 0, stsd)
	sttsBox := mp4FullBox("stts", 0, 0, []byte{0, 0, 0, 0})
	stscBox := mp4FullBox("stsc", 0, 0, []byte{0, 0, 0, 0})
	stszBox := mp4FullBox("stsz", 0, 0, make([]byte, 8))
	stcoBox := mp4FullBox("stco", 0, 0, []byte{0, 0, 0, 0})

	stbl := append([]byte{}, stsdBox...)
	stbl = append(stbl, sttsBox...)
	stbl = append(stbl, stscBox...)
	stbl = append(stbl, stszBox...)
	stbl = append(stbl, stcoBox...)
	stblBox := mp4Box("stbl", stbl)

	vmhdBox := mp4FullBox("vmhd", 0, 1, make([]byte, 8))
	drefEntry := mp4FullBox("url ", 0, 1, nil)
	dref := u32be(nil, 1)
	dref = append(dref, drefEntry...)
	dinfBox := mp4Box("dinf", mp4Box("dref", dref))

	minf := append([]byte{}, vmhdBox...)
	minf = append(minf, dinfBox...)
	minf = append(minf, stblBox...)
	minfBox := mp4Box("minf", minf)

	// mdhd (timescale 90 kHz)
	mdhd := u32be(nil, 0)
	mdhd = u32be(mdhd, 0)
	mdhd = u32be(mdhd, 90000)
	mdhd = u32be(mdhd, 0)
	mdhd = u16be(mdhd, 0x55C4)
	mdhd = u16be(mdhd, 0)
	mdhdBox := mp4FullBox("mdhd", 0, 0, mdhd)

	hdlr := u32be(nil, 0)
	hdlr = append(hdlr, "vide"...)
	hdlr = append(hdlr, make([]byte, 12)...)
	hdlr = append(hdlr, "VideoHandler\x00"...)
	hdlrBox := mp4FullBox("hdlr", 0, 0, hdlr)

	mdia := append([]byte{}, mdhdBox...)
	mdia = append(mdia, hdlrBox...)
	mdia = append(mdia, minfBox...)
	mdiaBox := mp4Box("mdia", mdia)

	// tkhd
	tkhd := u32be(nil, 0)
	tkhd = u32be(tkhd, 0)
	tkhd = u32be(tkhd, 1) // track_id
	tkhd = u32be(tkhd, 0)
	tkhd = u32be(tkhd, 0)
	tkhd = append(tkhd, make([]byte, 8)...)
	tkhd = u16be(tkhd, 0)
	tkhd = u16be(tkhd, 0)
	tkhd = u16be(tkhd, 0)
	tkhd = append(tkhd, 0, 0)
	tkhd = append(tkhd, fmp4Matrix...)
	tkhd = u32be(tkhd, width<<16)
	tkhd = u32be(tkhd, height<<16)
	tkhdBox := mp4FullBox("tkhd", 0, 7, tkhd)

	trak := append([]byte{}, tkhdBox...)
	trak = append(trak, mdiaBox...)
	trakBox := mp4Box("trak", trak)

	// mvhd
	mvhd := u32be(nil, 0)
	mvhd = u32be(mvhd, 0)
	mvhd = u32be(mvhd, 90000)
	mvhd = u32be(mvhd, 0)
	mvhd = u32be(mvhd, 0x00010000) // rate=1.0
	mvhd = u16be(mvhd, 0x0100)     // volume=1.0
	mvhd = append(mvhd, make([]byte, 10)...)
	mvhd = append(mvhd, fmp4Matrix...)
	mvhd = append(mvhd, make([]byte, 24)...)
	mvhd = u32be(mvhd, 2) // next_track_id
	mvhdBox := mp4FullBox("mvhd", 0, 0, mvhd)

	// trex (default values for fragments)
	trex := u32be(nil, 1) // track_id
	trex = u32be(trex, 1) // default_sample_description_index
	trex = u32be(trex, 0) // default_sample_duration
	trex = u32be(trex, 0) // default_sample_size
	trex = u32be(trex, 0x02000000)
	trexBox := mp4FullBox("trex", 0, 0, trex)
	mvexBox := mp4Box("mvex", trexBox)

	moov := append([]byte{}, mvhdBox...)
	moov = append(moov, trakBox...)
	moov = append(moov, mvexBox...)
	moovBox := mp4Box("moov", moov)

	init := make([]byte, 0, len(ftypBox)+len(moovBox))
	init = append(init, ftypBox...)
	return append(init, moovBox...)
}

// buildMediaSegment builds a moof+mdat fragment for one access unit.
func buildMediaSegment(nalus [][]byte, sequence uint32, timestamp uint64, duration uint32, isKey bool) []byte {
	// AVCC sample data (4-byte length prefix per NALU, skip AUD type 9).
	mdatData := make([]byte, 0, 1024)
	for _, nalu := range nalus {
		if len(nalu) > 0 && nalu[0]&0x1F == 9 {
			continue
		}
		mdatData = u32be(mdatData, uint32(len(nalu)))
		mdatData = append(mdatData, nalu...)
	}
	sampleSize := uint32(len(mdatData))

	mfhdBox := mp4FullBox("mfhd", 0, 0, u32be(nil, sequence))
	tfhdBox := mp4FullBox("tfhd", 0, 0x020000, u32be(nil, 1)) // default-base-is-moof
	tfdtBox := mp4FullBox("tfdt", 1, 0, u64be(nil, timestamp))

	// moof total size (see the Rust original for the derivation):
	// moof: 8 + mfhd(16) + traf(76) = 100; +8 for the mdat header.
	const dataOffset = 100 + 8

	trun := u32be(nil, 1) // sample_count
	trun = u32be(trun, dataOffset)
	trun = u32be(trun, duration)
	trun = u32be(trun, sampleSize)
	if isKey {
		trun = u32be(trun, 0x02000000)
	} else {
		trun = u32be(trun, 0x01010000)
	}
	trunBox := mp4FullBox("trun", 0, 0x000701, trun)

	traf := append([]byte{}, tfhdBox...)
	traf = append(traf, tfdtBox...)
	traf = append(traf, trunBox...)
	trafBox := mp4Box("traf", traf)

	moof := append([]byte{}, mfhdBox...)
	moof = append(moof, trafBox...)
	moofBox := mp4Box("moof", moof)
	mdatBox := mp4Box("mdat", mdatData)

	seg := make([]byte, 0, len(moofBox)+len(mdatBox))
	seg = append(seg, moofBox...)
	return append(seg, mdatBox...)
}
