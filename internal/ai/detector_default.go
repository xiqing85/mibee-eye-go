//go:build !ai

package ai

// Detector factory for builds without the `ai` tag: inference is
// unavailable and the service reports ai:false (fail-open, no fake data).

// newDetector always reports "not built with -tags ai" in tag-less builds.
func newDetector(Options) (Detector, error) {
	return nil, errNotBuilt
}

// NewDetector is the exported factory used by cmd/server wiring.
func NewDetector(opts Options) (Detector, error) { return newDetector(opts) }
