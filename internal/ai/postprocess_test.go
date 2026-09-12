package ai

import (
	"math"
	"testing"
)

// Real-world case: 1280×720 frame stretched into a 320×320 model input
// (x scale 4.0, y scale 2.25).
func TestScaleMapsModelBoxToVideoPixels(t *testing.T) {
	dets := []Detection{{Label: "chair", Confidence: 0.65, BBox: [4]uint32{272, 272, 48, 48}}}
	scaled := scaleDetectionsToFrame(dets, 320, 320, 1280, 720)
	if scaled[0].BBox != [4]uint32{1088, 612, 192, 108} {
		t.Fatalf("bbox = %v, want [1088 612 192 108]", scaled[0].BBox)
	}
}

func TestScaleIdentityWhenSizesMatch(t *testing.T) {
	dets := []Detection{{Label: "person", Confidence: 0.9, BBox: [4]uint32{10, 20, 30, 40}}}
	scaled := scaleDetectionsToFrame(dets, 320, 320, 320, 320)
	if scaled[0].BBox != [4]uint32{10, 20, 30, 40} {
		t.Fatalf("bbox = %v", scaled[0].BBox)
	}
}

func TestScaleClampsToFrameBounds(t *testing.T) {
	dets := []Detection{{Label: "person", Confidence: 0.9, BBox: [4]uint32{300, 300, 30, 30}}}
	scaled := scaleDetectionsToFrame(dets, 320, 320, 1280, 720)
	x, y, w, h := scaled[0].BBox[0], scaled[0].BBox[1], scaled[0].BBox[2], scaled[0].BBox[3]
	if x != 1200 || y != 675 {
		t.Fatalf("origin = (%d,%d), want (1200,675)", x, y)
	}
	if y+h > 720 || x+w > 1280 {
		t.Fatalf("box exceeds frame: (%d,%d,%d,%d)", x, y, w, h)
	}
}

// Class 0 ("person") at grid (x=20, y=20) on the stride-8 level
// (point 820), one-hot regression at distances (2, 3, 4, 5) → bbox
// (144, 136, 192, 200).
func syntheticOutputWithDetection() []float32 {
	output := make([]float32, gridForInput(320).points*numChannels)
	base := (20*40 + 20) * numChannels
	output[base] = 0.9
	for class := 1; class < numClasses; class++ {
		output[base+class] = 0.01
	}
	for side, distance := range []int{2, 3, 4, 5} {
		output[base+numClasses+side*numBins+distance] = 20.0
	}
	return output
}

func TestSyntheticDetectionDecodesBBoxWithin2px(t *testing.T) {
	detections, err := postprocess(syntheticOutputWithDetection(), gridForInput(320), 0.4)
	if err != nil {
		t.Fatalf("postprocess: %v", err)
	}
	if len(detections) != 1 {
		t.Fatalf("detections = %d, want 1", len(detections))
	}
	d := detections[0]
	if d.Label != "person" {
		t.Errorf("label = %q, want person", d.Label)
	}
	if math.Abs(float64(d.Confidence-0.9)) > 1e-6 {
		t.Errorf("confidence = %f, want 0.9", d.Confidence)
	}
	for i, want := range []int{144, 136, 48, 64} {
		got := int(d.BBox[i])
		if abs(got-want) > 2 {
			t.Errorf("bbox[%d] = %d, want ≈%d", i, got, want)
		}
	}
}

func TestAllZeroOutputYieldsNoDetections(t *testing.T) {
	output := make([]float32, gridForInput(320).points*numChannels)
	detections, err := postprocess(output, gridForInput(320), 0.4)
	if err != nil {
		t.Fatalf("postprocess: %v", err)
	}
	if len(detections) != 0 {
		t.Fatalf("detections = %d, want 0", len(detections))
	}
}

func TestShortOutputReturnsError(t *testing.T) {
	output := make([]float32, gridForInput(320).points*numChannels-1)
	if _, err := postprocess(output, gridForInput(320), 0.4); err == nil {
		t.Fatal("short output must be rejected")
	}
}

// The nanodet-plus-m_416.onnx export emits [1, 3598, 112]:
// ceil(416/8)² + ceil(416/16)² + ceil(416/32)² + ceil(416/64)²
// = 52² + 26² + 13² + 7² = 3598.
func TestGridForInput416MatchesOnnxExport(t *testing.T) {
	grid := gridForInput(416)
	if grid.points != 3598 {
		t.Fatalf("points = %d, want 3598", grid.points)
	}
	for _, c := range []struct {
		idx                    int
		level, gridX, gridY    int
		stride                 uint32
	}{
		{0, 0, 0, 0, 8},
		{52*52 - 1, 0, 51, 51, 8},
		{52 * 52, 1, 0, 0, 16},
		{52*52 + 26*26, 2, 0, 0, 32},
		{52*52 + 26*26 + 13*13, 3, 0, 0, 64},
		{3597, 3, 6, 6, 64},
	} {
		level, stride, gx, gy := grid.coords(c.idx)
		if level != c.level || stride != c.stride || gx != c.gridX || gy != c.gridY {
			t.Errorf("coords(%d) = (%d,%d,%d,%d), want (%d,%d,%d,%d)",
				c.idx, level, stride, gx, gy, c.level, c.stride, c.gridX, c.gridY)
		}
	}

	// A 320-sized tensor must NOT decode as a 416 grid (and vice versa).
	if _, err := postprocess(make([]float32, 2125*numChannels), grid, 0.4); err == nil {
		t.Fatal("320-length output must be rejected for a 416 grid")
	}
	if _, err := postprocess(make([]float32, 3598*numChannels), grid, 0.4); err != nil {
		t.Fatalf("416-length output must decode: %v", err)
	}
}

// Same one-hot construction on the 416 grid's stride-8 level:
// grid (x=30, y=25) → point 25*52+30; distances (2,3,4,5)
// → bbox ((30-2)*8, (25-3)*8, (30+4)*8-(30-2)*8, (25+5)*8-(25-3)*8)
// = (224, 176, 50, 64).
func TestSyntheticDetectionDecodesOn416Grid(t *testing.T) {
	grid := gridForInput(416)
	output := make([]float32, grid.points*numChannels)
	base := (25*52 + 30) * numChannels
	output[base] = 0.9
	for class := 1; class < numClasses; class++ {
		output[base+class] = 0.01
	}
	for side, distance := range []int{2, 3, 4, 5} {
		output[base+numClasses+side*numBins+distance] = 20.0
	}
	detections, err := postprocess(output, grid, 0.4)
	if err != nil {
		t.Fatalf("postprocess: %v", err)
	}
	if len(detections) != 1 {
		t.Fatalf("detections = %d, want 1", len(detections))
	}
	for i, want := range []int{224, 176, 50, 64} {
		got := int(detections[0].BBox[i])
		if abs(got-want) > 2 {
			t.Errorf("bbox[%d] = %d, want ≈%d", i, got, want)
		}
	}
}

func TestGridCoords(t *testing.T) {
	cases := []struct {
		idx             int
		level, gridX, y int
		stride          uint32
	}{
		{0, 0, 0, 0, 8},
		{820, 0, 20, 20, 8},
		{1599, 0, 39, 39, 8},
		{1600, 1, 0, 0, 16},
		{2000, 2, 0, 0, 32},
		{2100, 3, 0, 0, 64},
		{2124, 3, 4, 4, 64},
	}
	grid := gridForInput(320)
	for _, c := range cases {
		level, stride, gx, gy := grid.coords(c.idx)
		if level != c.level || stride != c.stride || gx != c.gridX || gy != c.y {
			t.Errorf("coords(%d) = (%d,%d,%d,%d), want (%d,%d,%d,%d)",
				c.idx, level, stride, gx, gy, c.level, c.stride, c.gridX, c.y)
		}
	}
}

func TestNMSSuppressesSameClassOverlap(t *testing.T) {
	candidates := []candidate{
		{label: 0, confidence: 0.9, x1: 10, y1: 10, x2: 100, y2: 100},
		{label: 0, confidence: 0.8, x1: 20, y1: 20, x2: 110, y2: 110},
	}
	kept := nms(candidates, 0.5)
	if len(kept) != 1 || kept[0].confidence != 0.9 {
		t.Fatalf("kept = %+v", kept)
	}
}

func TestNMSKeepsDifferentClasses(t *testing.T) {
	candidates := []candidate{
		{label: 0, confidence: 0.9, x1: 10, y1: 10, x2: 100, y2: 100},
		{label: 2, confidence: 0.8, x1: 20, y1: 20, x2: 110, y2: 110},
	}
	kept := nms(candidates, 0.5)
	if len(kept) != 2 {
		t.Fatalf("kept = %d, want 2", len(kept))
	}
}

func TestCocoLabels(t *testing.T) {
	if len(cocoLabels) != 80 {
		t.Fatalf("len = %d", len(cocoLabels))
	}
	for i, want := range map[int]string{0: "person", 2: "car", 16: "dog", 79: "toothbrush"} {
		if cocoLabels[i] != want {
			t.Errorf("cocoLabels[%d] = %q, want %q", i, cocoLabels[i], want)
		}
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
