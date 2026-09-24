// yuv_transform.go: YU12 (I420 planar 4:2:0) device-level transforms —
// flips and quarter-turn rotation baked into the raw frames before
// encoding (SPEC appendix A #9/#19). Used by the v4l2 backend; the
// libcamera subprocess backends bake the same transforms via rpicam-apps
// flags instead.

package camera

// RotatedDims returns the frame dimensions after baking rotation (SPEC
// appendix A #19): 90°/270° swap the axes. Values outside 0|90|180|270
// behave as 0 — config validation rejects them upstream.
func RotatedDims(width, height, rotation int) (int, int) {
	switch rotation {
	case 90, 270:
		return height, width
	default:
		return width, height
	}
}

// ComposeRotationFlips folds static rotation into per-frame flip flags
// (#19 order: rotate first, flips act on the rotated axes). 180° is the
// same group element as hflip+vflip, so it folds into the flips and
// costs nothing extra; 90°/270° stay as a transpose.
func ComposeRotationFlips(rotation int, hflip, vflip bool) (int, bool, bool) {
	switch rotation {
	case 180:
		return 0, !hflip, !vflip
	case 90, 270:
		return rotation, hflip, vflip
	default:
		return 0, hflip, vflip
	}
}

// NormalizeRotation maps any input to the supported quarter turns,
// treating everything else as 0 (defensive; validation rejects earlier).
func NormalizeRotation(degrees int) int {
	switch degrees {
	case 90, 180, 270:
		return degrees
	default:
		return 0
	}
}

// RotateYU12 rotates a YU12 frame 90° (clockwise) or 270°
// (counter-clockwise). Rotation cannot happen in place, so the rotated
// planes are written into *scratch and the two slices are swapped — on
// return *buf holds the rotated frame. Returns the rotated dimensions.
// Too-short buffers are left untouched. 0/180 are no-ops here (180 is
// handled via the flips).
func RotateYU12(buf, scratch *[]byte, width, height, rotation int) (int, int) {
	if rotation != 90 && rotation != 270 {
		return width, height
	}
	clockwise := rotation == 90
	ySize := width * height
	cw := (width + 1) / 2
	ch := (height + 1) / 2
	if width == 0 || height == 0 || len(*buf) < ySize+2*cw*ch {
		return height, width
	}
	if cap(*scratch) < len(*buf) {
		*scratch = make([]byte, len(*buf))
	}
	out := (*scratch)[:len(*buf)]
	cSize := cw * ch
	transposePlane((*buf)[:ySize], out[:ySize], width, height, clockwise)
	transposePlane((*buf)[ySize:ySize+cSize], out[ySize:ySize+cSize], cw, ch, clockwise)
	transposePlane((*buf)[ySize+cSize:], out[ySize+cSize:], cw, ch, clockwise)
	*buf, *scratch = out, *buf
	return height, width
}

// transposePlane transposes one plane (dims pw×ph) into dst laid out as
// ph×pw. Clockwise maps src(sx, sy) → dst(ph-1-sy, sx); counter-clockwise
// maps src(sx, sy) → dst(sy, pw-1-sx).
func transposePlane(src, dst []byte, pw, ph int, clockwise bool) {
	for sy := 0; sy < ph; sy++ {
		row := src[sy*pw : (sy+1)*pw]
		if clockwise {
			for sx, v := range row {
				dst[sx*ph+(ph-1-sy)] = v
			}
		} else {
			for sx, v := range row {
				dst[(pw-1-sx)*ph+sy] = v
			}
		}
	}
}

// FlipYU12 mirrors a YU12 frame in place (device-level hflip/vflip, SPEC
// appendix A #9). Too-short buffers are left untouched.
func FlipYU12(buf []byte, width, height int, hflip, vflip bool, scratch []byte) {
	if !hflip && !vflip {
		return
	}
	ySize := width * height
	cw := (width + 1) / 2
	ch := (height + 1) / 2
	if width == 0 || height == 0 || len(buf) < ySize+2*cw*ch {
		return
	}
	if len(scratch) < width {
		scratch = make([]byte, width)
	}
	flipPlane(buf[:ySize], width, height, hflip, vflip, scratch)
	flipPlane(buf[ySize:ySize+cw*ch], cw, ch, hflip, vflip, scratch)
	flipPlane(buf[ySize+cw*ch:], cw, ch, hflip, vflip, scratch)
}

// flipPlane mirrors one plane in place; scratch is one row wide and
// reused per row swap.
func flipPlane(p []byte, w, h int, hflip, vflip bool, scratch []byte) {
	row := scratch[:w]
	if vflip {
		top, bottom := 0, (h-1)*w
		for top < bottom {
			copy(row, p[top:top+w])
			mirrorCopy(p[top:top+w], p[bottom:bottom+w], hflip) // top = mirror(bottom orig)
			mirrorCopy(p[bottom:bottom+w], row, hflip)          // bottom = mirror(top orig)
			top += w
			bottom -= w
		}
		// Odd height: the middle row only needs internal mirroring.
		if top == bottom && hflip {
			mirrorRow(p[top : top+w])
		}
	} else if hflip {
		for off := 0; off < len(p); off += w {
			mirrorRow(p[off : off+w])
		}
	}
}

func mirrorRow(r []byte) {
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
}

func mirrorCopy(dst, src []byte, mirror bool) {
	if mirror {
		for i := range dst {
			dst[i] = src[len(src)-1-i]
		}
	} else {
		copy(dst, src)
	}
}
