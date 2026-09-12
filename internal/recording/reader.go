package recording

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/h264"
)

// nominalFPS is the fallback frame rate used when a segment has no
// sidecar (legacy or hand-made files).
const nominalFPS = 25

// SegmentReader yields access-unit-shaped frames from a recorded
// Annex-B segment file, pairing each frame with its recorded PTS from
// the .ts.jsonl sidecar. If the sidecar is missing, it falls back to a
// nominal 25fps pacing.
type SegmentReader struct {
	aus     [][]h264.NALU // access units, in order
	pts     []time.Duration
	nominal bool // true when using nominal fps fallback
	idx     int
}

// OpenSegment parses an Annex-B segment file into access units and
// loads its PTS sidecar.
func OpenSegment(file string) (*SegmentReader, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("reading segment %s: %w", file, err)
	}
	parser := h264.NewParser()
	nalus := parser.Parse(data)

	var aus [][]h264.NALU
	for _, n := range nalus {
		if len(aus) == 0 || startsNewAU(aus[len(aus)-1], n) {
			aus = append(aus, []h264.NALU{n})
		} else {
			aus[len(aus)-1] = append(aus[len(aus)-1], n)
		}
	}

	pts, ok := loadSidecar(file + ".ts.jsonl")
	return &SegmentReader{
		aus:     aus,
		pts:     pts,
		nominal: !ok,
	}, nil
}

// isVCL reports whether a NALU carries coded slice data (types 1 and 5).
func isVCL(n h264.NALU) bool { return n.Type == 1 || n.Type == 5 }

// startsNewAU reports whether n begins a new access unit given the
// current (in-progress) access unit cur. A new AU starts at a VCL
// NALU (slice) when the current AU already contains a VCL NALU.
// Non-VCL NALUs (SPS/PPS/SEI/AUD) attach to the following VCL NALU's
// AU, so a segment's leading SPS+PPS+IDR group into one AU.
func startsNewAU(cur []h264.NALU, n h264.NALU) bool {
	if !isVCL(n) {
		return false
	}
	for _, c := range cur {
		if isVCL(c) {
			return true
		}
	}
	return false
}

// loadSidecar reads a .ts.jsonl sidecar into a slice of PTS offsets.
// Returns ok=false if the file is missing or unreadable.
func loadSidecar(path string) ([]time.Duration, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	var pts []time.Duration
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec struct {
			PTSMS int64 `json:"pts_ms"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		pts = append(pts, time.Duration(rec.PTSMS)*time.Millisecond)
	}
	if len(pts) == 0 {
		return nil, false
	}
	return pts, true
}

// Next returns the next access unit as raw NALU byte slices (without
// start codes), its PTS offset relative to the segment start, and
// whether it is a keyframe. It returns io.EOF when exhausted.
func (r *SegmentReader) Next() (naluBytes [][]byte, ptsOffset time.Duration, isKeyFrame bool, err error) {
	if r.idx >= len(r.aus) {
		return nil, 0, false, errEOF
	}
	au := r.aus[r.idx]
	naluBytes = make([][]byte, len(au))
	for i, n := range au {
		naluBytes[i] = n.Data
		if n.IsIDR {
			isKeyFrame = true
		}
	}

	if r.nominal {
		ptsOffset = time.Duration(r.idx) * time.Second / nominalFPS
	} else if r.idx < len(r.pts) {
		ptsOffset = r.pts[r.idx]
	} else {
		// Sidecar shorter than the parsed AUs — fall back to nominal.
		ptsOffset = time.Duration(r.idx) * time.Second / nominalFPS
	}

	r.idx++
	return naluBytes, ptsOffset, isKeyFrame, nil
}

// errEOF is returned by Next when the segment is exhausted.
var errEOF = fmt.Errorf("recording: end of segment")
