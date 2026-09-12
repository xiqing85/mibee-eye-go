package ai

// YOLOX family pre/post-processing (registry family "yolox"), mirroring
// mibee-eye-raspi-rs so all MiBee cameras decode identically:
//
//   - Preprocess: RGB24 letterbox into the square input (aspect-preserving
//     resize, pad 114), /255 normalization, NCHW with RGB channel order.
//   - Output: [1, points, 85] raw predictions — 4 box deltas (grid-relative
//     xy, log-space wh), 1 objectness, 80 class logits. Grid: 3 FPN levels
//     at strides [8, 16, 32], floor(input/stride)² points each.
//   - Decode: x1y1 = (dxy + grid)*stride, wh = exp(dwh)*stride,
//     score = sigmoid(obj) * max(sigmoid(cls)); NMS at IoU 0.45.
//   - Unmap: letterbox offsets removed and the aspect scale inverted so
//     bboxes land in video pixel coordinates (SPEC v1 §4.6).

import (
	"fmt"
	"math"
	"sort"
)

const (
	yoloxNumClasses  = 80
	yoloxNumChannels = 4 + 1 + yoloxNumClasses
	yoloxNmsIOU      = 0.45
	// Letterbox padding gray level (YOLOX convention), normalized.
	yoloxPadValue = float32(114.0 / 255.0)
)

var yoloxStrides = [3]uint32{8, 16, 32}

// letterbox is the aspect-preserving geometry of the YOLOX preprocess.
type letterbox struct {
	scale float32
	padX  uint32
	padY  uint32
}

// yoloxGrid is the 3-level FPN layout derived from the input size (floor
// division — matching the official export, unlike NanoDet's 4-level ceil).
type yoloxGrid struct {
	input   uint32
	sizes   [3]int
	offsets [3]int
	points  int
}

func yoloxGridForInput(input uint32) yoloxGrid {
	var g yoloxGrid
	g.input = input
	for i, s := range yoloxStrides {
		g.sizes[i] = int(input / s)
	}
	for i := 1; i < len(g.offsets); i++ {
		g.offsets[i] = g.offsets[i-1] + g.sizes[i-1]*g.sizes[i-1]
	}
	g.points = g.offsets[2] + g.sizes[2]*g.sizes[2]
	return g
}

func (g yoloxGrid) coords(idx int) (level int, stride uint32, gridX, gridY int) {
	level = 0
	for l := len(g.offsets) - 1; l >= 0; l-- {
		if g.offsets[l] <= idx {
			level = l
			break
		}
	}
	local := idx - g.offsets[level]
	return level, yoloxStrides[level], local % g.sizes[level], local / g.sizes[level]
}

// preprocessYolox letterboxes an RGB24 frame into the square model input
// with /255 normalization, laid out NCHW in RGB channel order.
func preprocessYolox(rgb []byte, srcW, srcH, input uint32) ([]float32, letterbox, error) {
	if srcW == 0 || srcH == 0 || input == 0 {
		return nil, letterbox{}, fmt.Errorf("ai: zero-sized frame (%dx%d → %d)", srcW, srcH, input)
	}
	if uint64(len(rgb)) < uint64(srcW)*uint64(srcH)*3 {
		return nil, letterbox{}, fmt.Errorf("ai: RGB frame too short: got %d bytes, need %d (%dx%dx3)",
			len(rgb), uint64(srcW)*uint64(srcH)*3, srcW, srcH)
	}

	scale := min32(float32(input)/float32(srcW), float32(input)/float32(srcH))
	tw := uint32(math.Round(float64(float32(srcW) * scale)))
	if tw == 0 {
		tw = 1
	}
	th := uint32(math.Round(float64(float32(srcH) * scale)))
	if th == 0 {
		th = 1
	}
	padX := (input - tw) / 2
	padY := (input - th) / 2

	plane := uint64(input) * uint64(input)
	output := make([]float32, 3*plane)
	for i := uint64(0); i < plane; i++ {
		output[i], output[plane+i], output[2*plane+i] = yoloxPadValue, yoloxPadValue, yoloxPadValue
	}

	for dy := uint32(0); dy < th; dy++ {
		sy := uint32(float32(dy) / scale)
		if sy >= srcH {
			sy = srcH - 1
		}
		row := uint64(sy) * uint64(srcW)
		for dx := uint32(0); dx < tw; dx++ {
			sx := uint32(float32(dx) / scale)
			if sx >= srcW {
				sx = srcW - 1
			}
			idx := (row + uint64(sx)) * 3
			pixel := uint64(dy+padY)*uint64(input) + uint64(dx+padX)
			output[pixel] = float32(rgb[idx]) / 255
			output[plane+pixel] = float32(rgb[idx+1]) / 255
			output[2*plane+pixel] = float32(rgb[idx+2]) / 255
		}
	}

	return output, letterbox{scale: scale, padX: padX, padY: padY}, nil
}

func sigmoid32(x float32) float32 {
	return 1 / (1 + float32(math.Exp(-float64(x))))
}

// postprocessYolox decodes raw predictions into video-pixel detections,
// unmapping the letterbox geometry back to the frame (SPEC §4.6).
func postprocessYolox(output []float32, grid yoloxGrid, lb letterbox,
	videoW, videoH uint32, confidenceThreshold float32) ([]Detection, error) {
	expected := grid.points * yoloxNumChannels
	if len(output) != expected {
		return nil, fmt.Errorf("ai: unexpected YOLOX output length: got %d, expected %d (%d points × %d channels for input %d)",
			len(output), expected, grid.points, yoloxNumChannels, grid.input)
	}

	var candidates []candidate
	for idx := 0; idx < grid.points; idx++ {
		row := output[idx*yoloxNumChannels : (idx+1)*yoloxNumChannels]

		obj := sigmoid32(row[4])
		label := 0
		best := sigmoid32(row[5])
		for class := 1; class < yoloxNumClasses; class++ {
			if score := sigmoid32(row[5+class]); score > best {
				label = class
				best = score
			}
		}
		confidence := obj * best
		if confidence < confidenceThreshold {
			continue
		}

		_, stride, gridX, gridY := grid.coords(idx)
		inputF := float32(grid.input)
		x1 := max32((row[0]+float32(gridX))*float32(stride), 0)
		y1 := max32((row[1]+float32(gridY))*float32(stride), 0)
		x2 := min32(x1+float32(math.Exp(float64(row[2])))*float32(stride), inputF)
		y2 := min32(y1+float32(math.Exp(float64(row[3])))*float32(stride), inputF)

		inv := 1 / lb.scale
		fx1 := max32((x1-float32(lb.padX))*inv, 0)
		fy1 := max32((y1-float32(lb.padY))*inv, 0)
		fx2 := min32((x2-float32(lb.padX))*inv, float32(videoW))
		fy2 := min32((y2-float32(lb.padY))*inv, float32(videoH))

		candidates = append(candidates, candidate{
			label:      label,
			confidence: confidence,
			x1:         fx1,
			y1:         fy1,
			x2:         fx2,
			y2:         fy2,
		})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].confidence > candidates[j].confidence
	})
	kept := nms(candidates, yoloxNmsIOU)

	detections := make([]Detection, 0, len(kept))
	for _, c := range kept {
		detections = append(detections, Detection{
			Label:      cocoLabels[c.label],
			Confidence: c.confidence,
			BBox: [4]uint32{
				uint32(math.Round(float64(c.x1))),
				uint32(math.Round(float64(c.y1))),
				uint32(math.Round(float64(c.x2 - c.x1))),
				uint32(math.Round(float64(c.y2 - c.y1))),
			},
		})
	}
	return detections, nil
}
