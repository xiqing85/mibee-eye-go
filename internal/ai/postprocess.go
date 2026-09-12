package ai

// GFL (Generalized Focal Loss) post-processing for NanoDet ONNX models.
//
// Ported from mibee-eye-raspi-rs (same model, same numbers) so all MiBee
// cameras decode identically:
//
//  1. Classification — the first numClasses channels per point are class
//     scores (the nanodet-plus-m_320.onnx export folds the sigmoid into the
//     graph); argmax picks the label.
//  2. GFL regression — the remaining 4*(regMax+1) channels hold a discrete
//     distance distribution per box side (l, t, r, b); the expected
//     distance is dot(softmax(bins), [0..=regMax]) in stride units.
//  3. Bbox — x1 = (gridX - l)*stride, … clamped to the input frame.
//  4. NMS — per-class non-maximum suppression at IoU 0.5.
//
// Model output layout: [1, points, 112] (point-major) — `points` follows
// the model input size (see gridLayout): the 320 export emits 2125 points
// (40² + 20² + 10² + 5² at strides [8, 16, 32, 64]), the 416 export 3598
// (52² + 26² + 13² + 7²); 112 channels = 80 COCO classes + 32 regression.

import (
	"fmt"
	"math"
	"sort"
)

const (
	numClasses      = 80
	regMax          = 7
	numBins         = regMax + 1
	numRegression   = 4 * numBins
	numChannels     = numClasses + numRegression
	nmsIOUThreshold = 0.5
)

var strides = [4]uint32{8, 16, 32, 64}

// gridLayout is the FPN geometry derived from the model's (square) input
// size: each stride level contributes ceil(input/stride)² points, ordered
// level-major (stride 8 first) and row-major within a level — matching
// NanoDet's generate_grid_center_priors. Deriving the layout from the
// input size (instead of hardcoding the 320 export) is what lets one
// decoder serve every same-family model (320, 416, …).
type gridLayout struct {
	input   uint32
	sizes   [4]int
	offsets [4]int
	points  int
}

func gridForInput(input uint32) gridLayout {
	var g gridLayout
	g.input = input
	for i, s := range strides {
		g.sizes[i] = (int(input) + int(s) - 1) / int(s)
	}
	for i := 1; i < len(g.offsets); i++ {
		g.offsets[i] = g.offsets[i-1] + g.sizes[i-1]*g.sizes[i-1]
	}
	g.points = g.offsets[3] + g.sizes[3]*g.sizes[3]
	return g
}

// coords maps a flat point index to its (level, stride, gridX, gridY).
func (g gridLayout) coords(pointIdx int) (level int, stride uint32, gridX, gridY int) {
	level = 0
	for l := len(g.offsets) - 1; l >= 0; l-- {
		if g.offsets[l] <= pointIdx {
			level = l
			break
		}
	}
	local := pointIdx - g.offsets[level]
	gridW := g.sizes[level]
	return level, strides[level], local % gridW, local / gridW
}

// cocoLabels are the COCO 80-class labels in model output order.
var cocoLabels = [numClasses]string{
	"person", "bicycle", "car", "motorcycle", "airplane", "bus", "train",
	"truck", "boat", "traffic light", "fire hydrant", "stop sign",
	"parking meter", "bench", "bird", "cat", "dog", "horse", "sheep", "cow",
	"elephant", "bear", "zebra", "giraffe", "backpack", "umbrella",
	"handbag", "tie", "suitcase", "frisbee", "skis", "snowboard",
	"sports ball", "kite", "baseball bat", "baseball glove", "skateboard",
	"surfboard", "tennis racket", "bottle", "wine glass", "cup", "fork",
	"knife", "spoon", "bowl", "banana", "apple", "sandwich", "orange",
	"broccoli", "carrot", "hot dog", "pizza", "donut", "cake", "chair",
	"couch", "potted plant", "bed", "dining table", "toilet", "tv",
	"laptop", "mouse", "remote", "keyboard", "cell phone", "microwave",
	"oven", "toaster", "sink", "refrigerator", "book", "clock", "vase",
	"scissors", "teddy bear", "hair drier", "toothbrush",
}

// candidate is a decoded detection before NMS.
type candidate struct {
	label      int
	confidence float32
	x1, y1     float32
	x2, y2     float32
}

// postprocess decodes the flat [1, points, 112] output tensor into
// detections; the grid layout follows the model's actual input size.
// confidenceThreshold is the pre-NMS filter applied to the (already
// sigmoid'd) class scores.
func postprocess(output []float32, grid gridLayout, confidenceThreshold float32) ([]Detection, error) {
	expected := grid.points * numChannels
	if len(output) != expected {
		return nil, fmt.Errorf("ai: unexpected ONNX output length: got %d, expected %d (%d points × %d channels for input %d)",
			len(output), expected, grid.points, numChannels, grid.input)
	}

	var candidates []candidate
	for pointIdx := 0; pointIdx < grid.points; pointIdx++ {
		point := output[pointIdx*numChannels : (pointIdx+1)*numChannels]
		_, stride, gridX, gridY := grid.coords(pointIdx)

		// Classification: argmax over the class channels.
		label := 0
		confidence := point[0]
		for class := 1; class < numClasses; class++ {
			if point[class] > confidence {
				label = class
				confidence = point[class]
			}
		}
		if confidence < confidenceThreshold {
			continue
		}

		distances := decodeDistances(point[numClasses:])

		x1 := (float32(gridX) - distances[0]) * float32(stride)
		y1 := (float32(gridY) - distances[1]) * float32(stride)
		x2 := (float32(gridX) + distances[2]) * float32(stride)
		y2 := (float32(gridY) + distances[3]) * float32(stride)

		candidates = append(candidates, candidate{
			label:      label,
			confidence: confidence,
			x1:         max32(x1, 0),
			y1:         max32(y1, 0),
			x2:         min32(x2, float32(grid.input)),
			y2:         min32(y2, float32(grid.input)),
		})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].confidence > candidates[j].confidence
	})
	kept := nms(candidates, nmsIOUThreshold)

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

// scaleDetectionsToFrame maps bboxes from model-input pixel space back to
// the video frame's pixel space (SPEC v1 §4.6: bboxes are in video pixel
// coordinates). The preprocessing stretches the frame into the square
// model input without letterboxing, so x and y scale independently. Boxes
// are clamped to the frame bounds.
func scaleDetectionsToFrame(detections []Detection, modelW, modelH, frameW, frameH uint32) []Detection {
	if modelW == 0 || modelH == 0 || frameW == 0 || frameH == 0 {
		return detections
	}
	scaleX := float32(frameW) / float32(modelW)
	scaleY := float32(frameH) / float32(modelH)
	for i := range detections {
		x, y, w, h := detections[i].BBox[0], detections[i].BBox[1], detections[i].BBox[2], detections[i].BBox[3]
		x = min32u(uint32(math.Round(float64(float32(x)*scaleX))), frameW-1)
		y = min32u(uint32(math.Round(float64(float32(y)*scaleY))), frameH-1)
		w = min32u(uint32(math.Round(float64(float32(w)*scaleX))), frameW-x)
		h = min32u(uint32(math.Round(float64(float32(h)*scaleY))), frameH-y)
		detections[i].BBox = [4]uint32{x, y, w, h}
	}
	return detections
}

// decodeDistances decodes the 4 box-side distances (l, t, r, b) from the
// regression channels: each side's bins form a discrete distribution over
// distances 0..=regMax (stride units); the expected distance is the
// softmax-weighted sum.
func decodeDistances(regression []float32) [4]float32 {
	var distances [4]float32
	for side := 0; side < 4; side++ {
		bins := regression[side*numBins : (side+1)*numBins]
		weights := softmax(bins)
		for bin, weight := range weights {
			distances[side] += float32(bin) * weight
		}
	}
	return distances
}

// softmax is a numerically stable softmax over a slice.
func softmax(values []float32) []float32 {
	maxV := float32(math.Inf(-1))
	for _, v := range values {
		if v > maxV {
			maxV = v
		}
	}
	exps := make([]float32, len(values))
	var sum float32
	for i, v := range values {
		exps[i] = float32(math.Exp(float64(v - maxV)))
		sum += exps[i]
	}
	for i := range exps {
		exps[i] /= sum
	}
	return exps
}

// nms applies per-class non-maximum suppression. Candidates must be
// sorted by confidence descending.
func nms(candidates []candidate, iouThreshold float32) []candidate {
	kept := make([]candidate, 0, len(candidates))
	for _, c := range candidates {
		suppressed := false
		for _, k := range kept {
			if k.label == c.label && iou(k, c) > iouThreshold {
				suppressed = true
				break
			}
		}
		if !suppressed {
			kept = append(kept, c)
		}
	}
	return kept
}

func iou(a, b candidate) float32 {
	interW := min32(a.x2, b.x2) - max32(a.x1, b.x1)
	interH := min32(a.y2, b.y2) - max32(a.y1, b.y1)
	if interW <= 0 || interH <= 0 {
		return 0
	}
	inter := interW * interH
	union := (a.x2-a.x1)*(a.y2-a.y1) + (b.x2-b.x1)*(b.y2-b.y1) - inter
	return inter / union
}

func max32(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}

func min32(a, b float32) float32 {
	if a < b {
		return a
	}
	return b
}

func min32u(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}
