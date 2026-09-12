package ai

import (
	"math"
	"testing"
)

func TestYoloxGridForInput416MatchesOnnxExport(t *testing.T) {
	// The official yolox_nano.onnx emits [1, 3549, 85]: 52² + 26² + 13².
	grid := yoloxGridForInput(416)
	if grid.points != 3549 {
		t.Fatalf("points = %d, want 3549", grid.points)
	}
	for _, c := range []struct {
		idx                 int
		level, gridX, gridY int
		stride              uint32
	}{
		{0, 0, 0, 0, 8},
		{52*52 - 1, 0, 51, 51, 8},
		{52 * 52, 1, 0, 0, 16},
		{52*52 + 26*26, 2, 0, 0, 32},
		{3548, 2, 12, 12, 32},
	} {
		level, stride, gx, gy := grid.coords(c.idx)
		if level != c.level || stride != c.stride || gx != c.gridX || gy != c.gridY {
			t.Errorf("coords(%d) = (%d,%d,%d,%d), want (%d,%d,%d,%d)",
				c.idx, level, stride, gx, gy, c.level, c.stride, c.gridX, c.gridY)
		}
	}
}

func TestYoloxGridForInput320(t *testing.T) {
	grid := yoloxGridForInput(320)
	want := 40*40 + 20*20 + 10*10
	if grid.points != want {
		t.Fatalf("points = %d, want %d", grid.points, want)
	}
	if _, _, gx, gy := grid.coords(1600); gx != 0 || gy != 0 {
		t.Fatalf("coords(1600) = (%d,%d), want (0,0)", gx, gy)
	}
}

func TestYoloxLetterboxGeometry(t *testing.T) {
	// 16×9 source into 8×8: scale = 0.5 → content 8×round(4.5)=8×5,
	// pad (0, 1) — Go/Rust round half away from zero, like the YOLOX demo.
	frame := make([]byte, 16*9*3)
	tensor, lb := mustYoloxPreprocess(t, frame, 16, 9, 8)
	if len(tensor) != 3*8*8 {
		t.Fatalf("tensor len = %d", len(tensor))
	}
	if abs64(float64(lb.scale)-0.5) > 1e-4 || lb.padX != 0 || lb.padY != 1 {
		t.Fatalf("letterbox = %+v", lb)
	}
	// Padding pixel (top-left) is 114/255 in every plane.
	padPx := float32(114.0 / 255.0)
	for plane := 0; plane < 3; plane++ {
		if abs64(float64(tensor[plane*64]-padPx)) > 1e-4 {
			t.Fatalf("plane %d corner = %f, want %f", plane, tensor[plane*64], padPx)
		}
	}
}

func TestYoloxLetterboxRGBNormalization(t *testing.T) {
	// 4×4 white frame: content fills the square, RGB = 1.0 everywhere.
	frame := make([]byte, 4*4*3)
	for i := range frame {
		frame[i] = 255
	}
	tensor, lb := mustYoloxPreprocess(t, frame, 4, 4, 4)
	if lb.padX != 0 || lb.padY != 0 {
		t.Fatalf("letterbox = %+v", lb)
	}
	for plane := 0; plane < 3; plane++ {
		if abs64(float64(tensor[plane*16]-1.0)) > 1e-3 {
			t.Fatalf("plane %d = %f, want 1.0", plane, tensor[plane*16])
		}
	}
}

// silentOutput: every row's objectness suppressed. sigmoid(0) is 0.5, so
// zeros alone would decode as low-confidence detections — a real export
// never emits that, and tests must not either.
func silentYoloxOutput(grid yoloxGrid) []float32 {
	output := make([]float32, grid.points*yoloxNumChannels)
	for idx := 0; idx < grid.points; idx++ {
		output[idx*yoloxNumChannels+4] = -20.0
	}
	return output
}

// One known prediction on the 416 grid's stride-8 level:
// grid (26, 20) → row 20*52+26 = 1066; dxy = 0, dwh = ln 2 →
// xy = (208, 160), wh = (16, 16); identity letterbox + 416 frame.
func seedYoloxRow(output []float32) {
	row := 20*52 + 26
	base := row * yoloxNumChannels
	output[base+0] = 0
	output[base+1] = 0
	output[base+2] = float32(math.Log(2))
	output[base+3] = float32(math.Log(2))
	output[base+4] = 20.0
	output[base+5] = 20.0 // class 0 logit → sigmoid ≈ 1
}

func TestYoloxSyntheticDecodesIdentity(t *testing.T) {
	grid := yoloxGridForInput(416)
	output := silentYoloxOutput(grid)
	seedYoloxRow(output)
	lb := letterbox{scale: 1, padX: 0, padY: 0}
	detections, err := postprocessYolox(output, grid, lb, 416, 416, 0.05)
	if err != nil {
		t.Fatalf("postprocess: %v", err)
	}
	if len(detections) != 1 {
		t.Fatalf("detections = %d, want 1", len(detections))
	}
	d := detections[0]
	if d.Label != "person" || d.Confidence < 0.99 {
		t.Fatalf("detection = %+v", d)
	}
	if d.BBox != [4]uint32{208, 160, 16, 16} {
		t.Fatalf("bbox = %v, want [208 160 16 16]", d.BBox)
	}
}

func TestYoloxLetterboxUnmapScalesBackToFrame(t *testing.T) {
	// Same box decoded through the 1280×720 letterbox: input-space
	// (208..224, 160..176) → /0.325 → ≈(640..689, 212..261).
	grid := yoloxGridForInput(416)
	output := silentYoloxOutput(grid)
	seedYoloxRow(output)
	lb := letterbox{scale: 0.325, padX: 0, padY: 91}
	detections, err := postprocessYolox(output, grid, lb, 1280, 720, 0.05)
	if err != nil {
		t.Fatalf("postprocess: %v", err)
	}
	x, y, w, h := detections[0].BBox[0], detections[0].BBox[1], detections[0].BBox[2], detections[0].BBox[3]
	if x != 640 || y != 212 {
		t.Fatalf("origin = (%d,%d), want (640,212)", x, y)
	}
	if abs64(float64(w)-49) > 1 || abs64(float64(h)-49) > 1 {
		t.Fatalf("size = (%d,%d), want ≈(49,49)", w, h)
	}
}

func TestYoloxWrongLengthRejected(t *testing.T) {
	grid := yoloxGridForInput(416)
	lb := letterbox{scale: 1}
	wrong := make([]float32, 2125*yoloxNumChannels)
	if _, err := postprocessYolox(wrong, grid, lb, 416, 416, 0.05); err == nil {
		t.Fatal("a nanodet-sized tensor must not decode as yolox")
	}
}

func TestYoloxObjectnessGateSuppressesLowScore(t *testing.T) {
	grid := yoloxGridForInput(416)
	output := silentYoloxOutput(grid)
	row := 20*52 + 26
	base := row * yoloxNumChannels
	output[base+2] = float32(math.Log(2))
	output[base+3] = float32(math.Log(2))
	output[base+5] = 20.0 // high class score…
	output[base+4] = -20  // …but objectness ≈ 0

	lb := letterbox{scale: 1}
	detections, err := postprocessYolox(output, grid, lb, 416, 416, 0.05)
	if err != nil {
		t.Fatalf("postprocess: %v", err)
	}
	if len(detections) != 0 {
		t.Fatalf("detections = %d, want 0", len(detections))
	}
}

func abs64(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func mustYoloxPreprocess(t *testing.T, frame []byte, w, h, input uint32) ([]float32, letterbox) {
	t.Helper()
	tensor, lb, err := preprocessYolox(frame, w, h, input)
	if err != nil {
		t.Fatalf("preprocessYolox: %v", err)
	}
	return tensor, lb
}
