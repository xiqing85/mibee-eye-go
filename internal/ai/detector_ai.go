//go:build ai

package ai

// Detector factory for `ai`-tagged builds: ONNX Runtime detector.

func newDetector(opts Options) (Detector, error) {
	return newOrtDetector(opts)
}

// NewDetector is the exported factory used by cmd/server wiring.
func NewDetector(opts Options) (Detector, error) { return newDetector(opts) }
