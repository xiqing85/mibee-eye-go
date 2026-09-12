// Package ai provides on-device object detection (NanoDet-Plus ONNX).
//
// It mirrors the mibee-eye-raspi-rs pipeline so all MiBee cameras share
// one detection semantics: the same nanodet-plus-m_320 model, the same
// preprocessing constants, and the same GFL post-processing. Results are
// exposed through the unified Web API (SPEC v1 §4.6 GET /api/detections,
// ai_detection SSE events) with bboxes in video pixel coordinates.
//
// Frame source: this product's pipeline is H.264-only (no raw frames), so
// the decoder taps the AUHub and pipes H.264 into an ffmpeg subprocess
// that decodes keyframes only (-skip_frame nokey) into small RGB24 frames.
// Inference runs behind the `ai` build tag via onnxruntime_go (CGO +
// dlopen of libonnxruntime.so); builds without the tag get a stub
// detector and report ai:false.
package ai

// Detection is a single object detection in video pixel coordinates
// (SPEC v1 §4.6): bbox is [x, y, w, h], origin top-left, in the camera's
// native stream resolution.
type Detection struct {
	Label      string   `json:"label"`
	Confidence float32  `json:"confidence"`
	BBox       [4]uint32 `json:"bbox"`
}

// Snapshot is the response shape of GET /api/detections (SPEC v1 §4.6).
type Snapshot struct {
	Detections []Detection `json:"detections"`
	Model      string      `json:"model"`
	Timestamp  int64       `json:"timestamp"`
}

// Event is the SSE ai_detection payload (SPEC v1 §6).
type Event struct {
	CameraID    string      `json:"camera_id"`
	Detections  []Detection `json:"detections"`
	FrameNumber uint64      `json:"frame_number"`
}

// Frame is a decoded RGB24 frame (3 bytes per pixel, row-major).
type Frame struct {
	Width  uint32
	Height uint32
	Data   []byte
}

// Options configures the AI service.
type Options struct {
	// Enabled is the master switch ([ai] enabled).
	Enabled bool
	// Model is the registry id of the startup model (SPEC §4.6); an empty
	// value means "nanodet-plus-m-320".
	Model string
	// ModelPath points at the NanoDet ONNX model. A non-default value
	// overrides the registry id (custom deployments).
	ModelPath string
	// Family is the decoder family of Model ("nanodet" | "yolox"); the
	// service derives it from the registry when resolving models.
	Family string
	// OnnxLibPath points at libonnxruntime.so (empty = library search
	// path / system default).
	OnnxLibPath string
	// ConfidenceThreshold filters reported detections (0..1).
	ConfidenceThreshold float32
	// IntervalMs is the minimum spacing between inferences.
	IntervalMs uint64
	// DecoderBin is the ffmpeg binary used for keyframe decoding.
	DecoderBin string
	// VideoW/VideoH are the native stream dimensions (bbox output space).
	VideoW, VideoH uint32
}

func (o *Options) withDefaults() Options {
	c := *o
	if c.Model == "" {
		c.Model = "nanodet-plus-m-320"
	}
	if c.ModelPath == "" {
		c.ModelPath = "/var/lib/mibee-eye/models/nanodet-m.onnx"
	}
	if c.Family == "" {
		c.Family = "nanodet"
	}
	if c.ConfidenceThreshold <= 0 {
		c.ConfidenceThreshold = 0.35
	}
	if c.IntervalMs == 0 {
		c.IntervalMs = 1000
	}
	if c.DecoderBin == "" {
		c.DecoderBin = "ffmpeg"
	}
	return c
}

// Detector runs inference on one decoded frame.
type Detector interface {
	// Detect returns bboxes already scaled to the video resolution
	// (videoW x videoH).
	Detect(frame *Frame, videoW, videoH uint32) ([]Detection, error)
	// ModelName identifies the active model.
	ModelName() string
	// InputSize returns the square model input size in pixels (0 =
	// unknown); registry metadata for uploads (SPEC §4.6).
	InputSize() int
}

// Closer is the optional release hook detectors may implement: hot model
// swaps call Close on the replaced detector so native (ONNX Runtime C++)
// memory is freed immediately rather than at finalization.
type Closer interface {
	Close()
}

// stubDetector is returned when the binary lacks the `ai` build tag.
type stubDetector struct{}

func (stubDetector) Detect(*Frame, uint32, uint32) ([]Detection, error) {
	return nil, errNotBuilt
}

func (stubDetector) ModelName() string { return "" }

func (stubDetector) InputSize() int { return 0 }

// errNotBuilt is reported by builds without the `ai` tag.
var errNotBuilt = &notBuiltError{}

type notBuiltError struct{}

func (*notBuiltError) Error() string {
	return "ai: binary built without the `ai` tag; rebuild with -tags ai"
}
