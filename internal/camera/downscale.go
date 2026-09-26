package camera

// downscale.go — nearest-neighbour I420 (YU12) downscaler for the
// bandwidth-saving substream (SPEC appendix A #20). Go twin of the Rust
// implementation in mibee-eye-rs (src/camera/downscale.rs); the two must
// stay semantically identical (identity/upscale/short-input all return the
// input unchanged, never panic).
//
// The main pipeline bakes rotation/flips into its frames before encoding,
// so a tap placed after those transforms hands the substream fully
// finished pixels — the sub encoder inherits them for free.

import "sync"

// downscaleMaps caches the per-axis nearest-neighbour index tables per
// (dst, src) pair: the tables only depend on the dimension pair, and the
// substream pipeline reuses them for every frame.
var (
	downscaleMapsMu sync.Mutex
	downscaleMaps   = map[[2]uint32][]int{}
)

func mapAxis(dst, src uint32) []int {
	key := [2]uint32{dst, src}
	downscaleMapsMu.Lock()
	defer downscaleMapsMu.Unlock()
	if m, ok := downscaleMaps[key]; ok {
		return m
	}
	m := make([]int, dst)
	for i := range m {
		m[i] = int(uint64(i) * uint64(src) / uint64(dst))
	}
	downscaleMaps[key] = m
	return m
}

// DownscaleYU12 downscales a contiguous I420 frame (srcW×srcH, plane
// layout Y then U then V) to dstW×dstH.
//
// Behavior guarantees (mirroring the Rust twin):
//   - identity (dst == src dims) and no-op upscale (dst larger than src in
//     either axis) return a copy of the input unchanged;
//   - a src shorter than the I420 plane layout is returned unchanged;
//   - never panics: all indexing derives from the validated length.
func DownscaleYU12(src []byte, srcW, srcH, dstW, dstH uint32) []byte {
	srcY := srcW * srcH
	srcC := srcW / 2 * (srcH / 2)
	if srcW == 0 || srcH == 0 || dstW == 0 || dstH == 0 ||
		dstW > srcW || dstH > srcH ||
		uint64(len(src)) < uint64(srcY)+2*uint64(srcC) {
		out := make([]byte, len(src))
		copy(out, src)
		return out
	}
	if dstW == srcW && dstH == srcH {
		out := make([]byte, len(src))
		copy(out, src)
		return out
	}

	dstYu := int(dstW * dstH)
	dstCh := int(max(dstH/2, 1))
	dstCw := int(max(dstW/2, 1))
	dstC := dstCh * dstCw
	out := make([]byte, dstYu+2*dstC)

	xmap := mapAxis(dstW, srcW)
	ymap := mapAxis(dstH, srcH)
	srcCw := int(srcW / 2)

	// Luma.
	for dy, sy := range ymap {
		sRow := sy * int(srcW)
		dRow := dy * int(dstW)
		for dx, sx := range xmap {
			out[dRow+dx] = src[sRow+sx]
		}
	}

	// Chroma (half per axis; chroma dst row/col c pairs with luma dst
	// row/col 2c, mapped through the luma tables and halved). Clamped to
	// 1 so degenerate odd dimensions still produce a contiguous layout.
	lastRow := len(ymap) - 1
	lastCol := len(xmap) - 1
	for dcy := 0; dcy < dstCh; dcy++ {
		lumaRow := min(dcy*2, lastRow)
		sRowU := int(srcY) + ymap[lumaRow]/2*srcCw
		sRowV := sRowU + int(srcC)
		for dcx := 0; dcx < dstCw; dcx++ {
			lumaCol := min(dcx*2, lastCol)
			col := xmap[lumaCol] / 2
			out[dstYu+dcy*dstCw+dcx] = src[sRowU+col]
			out[dstYu+dstC+dcy*dstCw+dcx] = src[sRowV+col]
		}
	}
	return out
}
