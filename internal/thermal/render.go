package thermal

import (
	"fmt"
	"math"
	"sort"
)

// Ironbow control points (t, r, g, b) — same feel as the bring-up viewer.
var ironbowPts = [8][4]float32{
	{0.00, 0, 0, 0},
	{0.15, 18, 8, 64},
	{0.35, 64, 12, 120},
	{0.55, 140, 30, 130},
	{0.72, 216, 88, 60},
	{0.86, 248, 168, 48},
	{0.96, 255, 232, 120},
	{1.00, 255, 255, 224},
}

func clampf(v, lo, hi float32) float32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Ironbow maps t∈[0,1] to the pseudo-color ramp.
func Ironbow(t float32) (uint8, uint8, uint8) {
	t = clampf(t, 0, 1)
	for i := 0; i+1 < len(ironbowPts); i++ {
		p0, p1 := ironbowPts[i], ironbowPts[i+1]
		if t <= p1[0] {
			f := float32(0)
			if p1[0] > p0[0] {
				f = (t - p0[0]) / (p1[0] - p0[0])
			}
			return uint8(p0[1] + f*(p1[1]-p0[1])),
				uint8(p0[2] + f*(p1[2]-p0[2])),
				uint8(p0[3] + f*(p1[3]-p0[3]))
		}
	}
	last := ironbowPts[len(ironbowPts)-1]
	return uint8(last[1]), uint8(last[2]), uint8(last[3])
}

// Percentile stretch parameters.
const (
	pLo    = 0.02
	pHi    = 0.98
	gammaR = 0.7
)

// stretchRange resolves the display range: explicit lo/hi when both are
// given and sane, else the 2%/98% percentile stretch of the frame.
func stretchRange(to *[Pixels]float32, lo, hi *float32) (float32, float32) {
	if lo != nil && hi != nil && *hi-*lo > 1e-3 {
		return *lo, *hi
	}
	sorted := make([]float32, Pixels)
	copy(sorted, to[:])
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	n := float64(Pixels)
	loIdx := int(n * pLo)
	if loIdx > Pixels-1 {
		loIdx = Pixels - 1
	}
	hiIdx := int(n * pHi)
	if hiIdx > Pixels-1 {
		hiIdx = Pixels - 1
	}
	return sorted[loIdx], sorted[hiIdx]
}

// RenderRGBRange renders the To grid (bilinear upscale + ironbow) into a
// packed RGB buffer of out_w×out_h.
func RenderRGBRange(to *[Pixels]float32, outW, outH int, loOv, hiOv *float32) []byte {
	lo, hi := stretchRange(to, loOv, hiOv)
	span := hi - lo
	if span <= 1e-3 {
		span = 1
	}
	gridRGB := make([][3]uint8, Pixels)
	for i, v := range to {
		t := float32(math.Pow(float64(clampf((v-lo)/span, 0, 1)), 1/gammaR))
		r, g, b := Ironbow(t)
		gridRGB[i] = [3]uint8{r, g, b}
	}
	out := make([]byte, outW*outH*3)
	for y := 0; y < outH; y++ {
		gy := (float32(y)+0.5)*float32(Height)/float32(outH) - 0.5
		y0 := int(clampf(float32(math.Floor(float64(gy))), 0, float32(Height-1)))
		y1 := y0 + 1
		if y1 > Height-1 {
			y1 = Height - 1
		}
		fy := clampf(gy-float32(y0), 0, 1)
		for x := 0; x < outW; x++ {
			gx := (float32(x)+0.5)*float32(Width)/float32(outW) - 0.5
			x0 := int(clampf(float32(math.Floor(float64(gx))), 0, float32(Width-1)))
			x1 := x0 + 1
			if x1 > Width-1 {
				x1 = Width - 1
			}
			fx := clampf(gx-float32(x0), 0, 1)
			c00, c01, c10, c11 := gridRGB[y0*Width+x0], gridRGB[y0*Width+x1],
				gridRGB[y1*Width+x0], gridRGB[y1*Width+x1]
			lerp := func(a, b, f float32) float32 { return a + f*(b-a) }
			ch := func(v0, v1, v2, v3 uint8) uint8 {
				return uint8(clampf(lerp(lerp(float32(v0), float32(v1), fx),
					lerp(float32(v2), float32(v3), fx), fy), 0, 255))
			}
			o := (y*outW + x) * 3
			out[o] = ch(c00[0], c01[0], c10[0], c11[0])
			out[o+1] = ch(c00[1], c01[1], c10[1], c11[1])
			out[o+2] = ch(c00[2], c01[2], c10[2], c11[2])
		}
	}
	return out
}

// RenderRGBAnnotated renders with the on-image overlays (max/min/center
// brackets + labels + right color bar) — the thermal-camera look.
func RenderRGBAnnotated(to *[Pixels]float32, outW, outH int, loOv, hiOv *float32) []byte {
	lo, hi := stretchRange(to, loOv, hiOv)
	rgb := RenderRGBRange(to, outW, outH, loOv, hiOv)
	overlayTemperatures(to, rgb, outW, outH, lo, hi)
	return rgb
}

// RGBToBMP packs RGB into a minimal 24-bit BMP (rows padded, bottom-up).
func RGBToBMP(rgb []byte, w, h int) []byte {
	row := (w*3 + 3) & ^3
	size := 54 + row*h
	out := make([]byte, size)
	copy(out[0:2], "BM")
	le32(out[2:6], uint32(size))
	le32(out[10:14], 54)
	le32(out[14:18], 40)
	le32(out[18:22], uint32(w))
	le32(out[22:26], uint32(h))
	le16(out[26:28], 1)
	le16(out[28:30], 24)
	for y := 0; y < h; y++ {
		src := (h - 1 - y) * w * 3
		dst := 54 + y*row
		copy(out[dst:dst+w*3], rgb[src:src+w*3])
	}
	return out
}

func le32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

func le16(b []byte, v uint16) { b[0], b[1] = byte(v), byte(v>>8) }

// ————————————————————————————————————————————————————————————————
// On-image overlay: 5x7 bitmap font, 3× supersampled AA, brackets, bar.
// ————————————————————————————————————————————————————————————————

type glyphRows [7]byte

var font = map[rune]glyphRows{
	'0': {0x0E, 0x11, 0x13, 0x15, 0x19, 0x11, 0x0E},
	'1': {0x04, 0x0C, 0x04, 0x04, 0x04, 0x04, 0x0E},
	'2': {0x0E, 0x11, 0x01, 0x02, 0x04, 0x08, 0x1F},
	'3': {0x1F, 0x02, 0x04, 0x02, 0x01, 0x11, 0x0E},
	'4': {0x02, 0x06, 0x0A, 0x12, 0x1F, 0x02, 0x02},
	'5': {0x1F, 0x10, 0x1E, 0x01, 0x01, 0x11, 0x0E},
	'6': {0x06, 0x08, 0x10, 0x1E, 0x11, 0x11, 0x0E},
	'7': {0x1F, 0x01, 0x02, 0x04, 0x08, 0x08, 0x08},
	'8': {0x0E, 0x11, 0x11, 0x0E, 0x11, 0x11, 0x0E},
	'9': {0x0E, 0x11, 0x11, 0x0F, 0x01, 0x02, 0x0C},
	'.': {0x00, 0x00, 0x00, 0x00, 0x00, 0x0C, 0x0C},
	'-': {0x00, 0x00, 0x00, 0x1F, 0x00, 0x00, 0x00},
	'C': {0x0E, 0x11, 0x10, 0x10, 0x10, 0x11, 0x0E},
	'°': {0x06, 0x09, 0x06, 0x00, 0x00, 0x00, 0x00},
}

func blendPx(rgb []byte, p int, c [3]uint8, coverage uint8) {
	a := float32(coverage) / 255.0
	for i := 0; i < 3; i++ {
		cur := float32(rgb[p+i])
		rgb[p+i] = uint8(clampf(cur+(float32(c[i])-cur)*a+0.5, 0, 255))
	}
}

func textAdvance(text []rune, scale int) int {
	n := 0
	for _, c := range text {
		if c == '°' {
			n += 4
		} else {
			n += 6
		}
	}
	return n * scale
}

type textBox struct{ w, h, pad int }

func textBoxFor(text []rune, scale int) textBox {
	pad := 2 * scale
	return textBox{w: textAdvance(text, scale) + pad*2, h: 7*scale + pad*2, pad: pad}
}

// drawTextAA draws anti-aliased text on a rounded translucent dark box
// (65% darkening — readable everywhere without a solid slab).
func drawTextAA(rgb []byte, w, h, x, y int, text string, scale int, color [3]uint8) {
	chars := []rune(text)
	b := textBoxFor(chars, scale)
	const margin = 6
	if mx := w - (b.w + margin); x > mx {
		x = mx
	}
	if x < 0 {
		x = 0
	}
	if my := h - (b.h + margin); y > my {
		y = my
	}
	if y < 0 {
		y = 0
	}
	if b.w == 0 || b.h == 0 || x >= w || y >= h {
		return
	}

	// Rounded translucent background.
	radius := 2 * scale
	for dy := 0; dy < b.h && dy < h-y; dy++ {
		for dx := 0; dx < b.w && dx < w-x; dx++ {
			nx, ny := dx, dy
			if b.w-dx < nx {
				nx = b.w - dx
			}
			if radius < nx {
				nx = radius
			}
			if b.h-dy < ny {
				ny = b.h - dy
			}
			if radius < ny {
				ny = radius
			}
			if nx+ny < radius {
				continue // outside the rounded corner
			}
			p := ((y+dy)*w + (x + dx)) * 3
			for c := 0; c < 3; c++ {
				rgb[p+c] = uint8(float32(rgb[p+c]) * 0.35)
			}
		}
	}

	// Glyph pass with 3x3 supersampling per destination pixel.
	const ss = 3
	origin := 0
	for _, ch := range chars {
		rows, ok := font[ch]
		if !ok {
			if ch == '°' {
				origin += 4 * scale
			} else {
				origin += 6 * scale
			}
			continue
		}
		for gy := 0; gy < 7*scale; gy++ {
			for gx := 0; gx < 5*scale; gx++ {
				hits := 0
				for sy := 0; sy < ss; sy++ {
					for sx := 0; sx < ss; sx++ {
						fx := int(math.Floor((float64(gx) + float64(sx)/ss) / float64(scale)))
						fy := int(math.Floor((float64(gy) + float64(sy)/ss) / float64(scale)))
						if fx >= 0 && fx < 5 && fy >= 0 && fy < 7 &&
							rows[fy]&(1<<(4-fx)) != 0 {
							hits++
						}
					}
				}
				if hits == 0 {
					continue
				}
				coverage := uint8(hits * 255 / (ss * ss))
				px := x + b.pad + origin + gx
				py := y + b.pad + gy
				if px >= w || py >= h {
					continue
				}
				blendPx(rgb, (py*w+px)*3, color, coverage)
			}
		}
		if ch == '°' {
			origin += 4 * scale
		} else {
			origin += 6 * scale
		}
	}
}

// brackets draws FLIR-style thin L-corner brackets centered on a spot:
// 1px color core with a 1px dark outline.
func brackets(rgb []byte, w, h, cx, cy, s int, color [3]uint8) {
	half := int32(s / 2)
	arm := half + 1
	if arm > 6 {
		arm = 6
	}
	ccx, ccy := int32(cx), int32(cy)
	for _, pass := range []struct {
		col [3]uint8
		off int32
	}{{[3]uint8{0, 0, 0}, 1}, {color, 0}} {
		col, off := pass.col, pass.off
		for _, sq := range [4][2]int32{{-1, -1}, {1, -1}, {-1, 1}, {1, 1}} {
			sx, sy := sq[0], sq[1]
			for d := int32(0); d < arm; d++ {
				pts := [2][2]int32{
					{ccx + sx*(half+off) - sx*off, ccy + sy*(half+off) + sy*d - sy*off},
					{ccx + sx*(half+off) + sx*d - sx*off, ccy + sy*(half+off) - sy*off},
				}
				for _, pt := range pts {
					px, py := pt[0], pt[1]
					if px < 0 || py < 0 || px >= int32(w) || py >= int32(h) {
						continue
					}
					p := (int(py)*w + int(px)) * 3
					rgb[p], rgb[p+1], rgb[p+2] = col[0], col[1], col[2]
				}
			}
		}
	}
}

// overlayTemperatures bakes the instrument look onto a rendered frame:
// max/min/center brackets + labels and a right-side ironbow color bar.
func overlayTemperatures(to *[Pixels]float32, rgb []byte, w, h int, lo, hi float32) {
	scale := 1
	if w >= 480 {
		scale = 2
	}
	cellW := w / Width
	cellH := h / Height

	maxI, minI := 0, 0
	for i, v := range to {
		if v > to[maxI] {
			maxI = i
		}
		if v < to[minI] {
			minI = i
		}
	}
	label := func(v float32) string { return fmt.Sprintf("%.1f°C", v) }

	// Unified anchor: bracket on the cell, label right below it, flip above
	// when it would run off the bottom.
	mark := func(i int, color [3]uint8) {
		gx, gy := i%Width, i/Width
		cx := gx*cellW + cellW/2
		cy := gy*cellH + cellH/2
		sz := cellW * 3 / 2
		if sz < 6 {
			sz = 6
		}
		brackets(rgb, w, h, cx, cy, sz, color)
		lb := label(to[i])
		b := textBoxFor([]rune(lb), scale)
		lx := cx - b.w/2
		if lx < 0 {
			lx = 0
		}
		below := cy + cellH
		ly := below
		if below+b.h+4 > h {
			ly = cy - cellH - b.h
			if ly < 0 {
				ly = 0
			}
		}
		drawTextAA(rgb, w, h, lx, ly, lb, scale, [3]uint8{255, 255, 255})
	}

	white := [3]uint8{255, 255, 255}
	mark(maxI, white)
	mark(minI, [3]uint8{110, 235, 255})

	// Center spot: tight amber brackets + reading.
	cI := (Height/2)*Width + Width/2
	cx, cy := w/2, h/2
	barW := cellW
	if barW < 6 {
		barW = 6
	}
	brackets(rgb, w, h, cx, cy, barW, [3]uint8{255, 214, 92})
	clb := label(to[cI])
	cb := textBoxFor([]rune(clb), scale)
	cly := cy + cellH
	if my := h - (cb.h + 6); cly > my {
		cly = my
	}
	if cly < 0 {
		cly = 0
	}
	clx := cx - cb.w/2
	if clx < 0 {
		clx = 0
	}
	drawTextAA(rgb, w, h, clx, cly, clb, scale, white)

	// Right-side color bar with range labels.
	bw := scale * 8
	if bw < 12 {
		bw = 12
	}
	bh := h * 2 / 3
	bx := w - (bw + 10)
	if bx < 0 {
		bx = 0
	}
	by := (h - bh) / 2
	for dy := 0; dy < bh; dy++ {
		t := 1.0 - float32(dy)/float32(bh) // top = hot
		r, g, bl := Ironbow(t)
		for dx := 0; dx < bw; dx++ {
			px, py := bx+dx, by+dy
			if px >= w || py >= h {
				continue
			}
			p := (py*w + px) * 3
			rgb[p], rgb[p+1], rgb[p+2] = r, g, bl
		}
	}
	for _, pass := range []struct {
		col [3]uint8
		off int32
	}{{[3]uint8{0, 0, 0}, 1}, {white, 0}} {
		for d := 0; d <= bh; d++ {
			for _, px := range []int32{int32(bx) - pass.off, int32(bx+bw) + pass.off - 1} {
				py := int32(by) + int32(d) - pass.off
				if px < 0 || py < 0 || px >= int32(w) || py >= int32(h) {
					continue
				}
				p := (int(py)*w + int(px)) * 3
				rgb[p], rgb[p+1], rgb[p+2] = pass.col[0], pass.col[1], pass.col[2]
			}
		}
	}
	mid := (lo + hi) / 2.0
	ticks := []struct {
		v  float32
		ty int
	}{{hi, by}, {mid, by + bh/2}, {lo, by + bh}}
	for _, tk := range ticks {
		text := fmt.Sprintf("%.1f°", tk.v)
		b := textBoxFor([]rune(text), scale)
		tx := bx - (b.w + 6)
		if tx < 0 {
			tx = 0
		}
		ty := tk.ty - b.h/2
		if ty < 0 {
			ty = 0
		}
		drawTextAA(rgb, w, h, tx, ty, text, scale, white)
	}
}
