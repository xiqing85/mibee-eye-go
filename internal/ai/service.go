package ai

// Service wires the frame decoder, the detector and the API surface
// together: it runs at most one inference per interval, keeps the latest
// snapshot for GET /api/detections, and publishes ai_detection events for
// the SSE hub. Fail-open everywhere: a missing model or ONNX Runtime
// library leaves the service inactive (ai:false), never fake data.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/h264"
)

// Service runs the detection loop for one camera.
type Service struct {
	detector    Detector
	newDetector func(Options) (Detector, error)
	decoder     *FrameDecoder
	opts        Options
	mu          sync.RWMutex
	snap        Snapshot
	events      chan Event
	inference   atomic.Uint64
	frame       atomic.Uint64
	active      bool
	modelID     string
}

// NewService builds the AI service from options. It returns (nil, nil)
// when disabled or unavailable (fail-open) — callers treat nil as
// ai:false. The detector factory is injected so tests can stub inference.
func NewService(opts Options, hub *h264.AUHub, newDetector func(Options) (Detector, error)) *Service {
	opts = opts.withDefaults()
	if !opts.Enabled {
		slog.Info("ai: disabled by configuration")
		return nil
	}
	model, ok := Resolve(opts.Model, opts.ModelPath)
	if !ok {
		slog.Warn("ai: invalid model configuration, AI stays disabled (fail-open)",
			"model", opts.Model)
		return nil
	}
	if !model.Custom {
		opts.Model = model.ID
		opts.ModelPath = model.Path
		opts.Family = model.Family
	}
	detector, err := newDetector(opts)
	if err != nil {
		slog.Warn("ai: detector unavailable, AI stays disabled (fail-open)", "error", err)
		return nil
	}
	return &Service{
		detector:    detector,
		newDetector: newDetector,
		decoder:     NewFrameDecoder(hub, opts.DecoderBin),
		opts:        opts,
		events:      make(chan Event, 16),
		active:      true,
		modelID:     model.ID,
		snap:        Snapshot{Detections: []Detection{}, Model: model.ID},
	}
}

// Active reports whether a real detector is loaded.
func (s *Service) Active() bool { return s != nil && s.active }

// ModelID identifies the active model id (SPEC §4.6; "" when inactive).
func (s *Service) ModelID() string {
	if !s.Active() {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.modelID
}

// ValidateModel fully loads a session for the declared family — the
// upload gate (SPEC §4.6): only models that load enter the registry. Uses
// the injected factory, so tests validate with fakes. Returns the model's
// input size for the registry entry.
func (s *Service) ValidateModel(path, family string) (int, error) {
	if !s.Active() {
		return 0, fmt.Errorf("ai: service not active")
	}
	det, err := s.newDetector(Options{ModelPath: path, Family: family})
	if err != nil {
		return 0, err
	}
	return det.InputSize(), nil
}

// currentDetector returns the detector for this iteration (SPEC §4.6
// hot-swap): the slot is swapped under the write lock, so the loop always
// runs the model that is active now.
func (s *Service) currentDetector() Detector {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.detector
}

// ActivateModel hot-switches the running model (SPEC §4.6 activate). The
// new detector is fully built BEFORE the slot is touched, so a failed load
// leaves the old model running (rollback by construction). Stale snapshots
// from the previous model are cleared.
func (s *Service) ActivateModel(id string) error {
	spec, ok := Find(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownModel, id)
	}
	if !Available(spec.Path) {
		return fmt.Errorf("%w: %s", ErrModelUnavailable, spec.Path)
	}
	opts := s.opts
	opts.Model = id
	opts.ModelPath = spec.Path
	opts.Family = spec.Family
	detector, err := s.newDetector(opts)
	if err != nil {
		return fmt.Errorf("loading model %s: %w", id, err)
	}
	s.mu.Lock()
	old := s.detector
	s.detector = detector
	s.modelID = id
	s.snap = Snapshot{Detections: []Detection{}, Model: id, Timestamp: time.Now().Unix()}
	s.mu.Unlock()
	// Release the replaced session's native memory promptly (Pi 3B hygiene).
	if c, ok := old.(Closer); ok {
		c.Close()
	}
	slog.Info("ai: model activated", "model", id, "input", spec.Input)
	return nil
}

// ModelName identifies the active model ("" when inactive).
func (s *Service) ModelName() string {
	if !s.Active() {
		return ""
	}
	return s.detector.ModelName()
}

// Snapshot returns the latest detections (SPEC v1 §4.6 shape).
func (s *Service) Snapshot() Snapshot {
	if !s.Active() {
		return Snapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap
}

// Events exposes the ai_detection SSE stream (SPEC v1 §6). The channel is
// never closed; consumers must stop with the context.
func (s *Service) Events() <-chan Event { return s.events }

// Inferences returns the completed-inference counter (for /metrics).
func (s *Service) Inferences() uint64 {
	if !s.Active() {
		return 0
	}
	return s.inference.Load()
}

// Start runs the decode + inference loop until ctx is cancelled.
func (s *Service) Start(ctx context.Context) {
	if !s.Active() {
		return
	}
	slog.Info("ai: service started", "model", s.detector.ModelName(),
		"interval_ms", s.opts.IntervalMs, "threshold", s.opts.ConfidenceThreshold,
		"decoder", s.opts.DecoderBin,
		"frame", fmt.Sprintf("%dx%d", decoderFrameW, decoderFrameH))

	s.decoder.Start(ctx)
	go s.runLoop(ctx, s.decoder.Frames())
}

// runLoop consumes decoded frames and runs at most one inference per
// interval. Exposed for tests, which feed synthetic frame channels.
func (s *Service) runLoop(ctx context.Context, frames <-chan Frame) {
	interval := time.Duration(s.opts.IntervalMs) * time.Millisecond
	lastRun := time.Now().Add(-interval)
	for frame := range frames {
		if ctx.Err() != nil {
			return
		}
		if time.Since(lastRun) < interval {
			continue
		}
		lastRun = time.Now()
		frameNo := s.frame.Add(1)

		detections, err := s.currentDetector().Detect(&frame, s.opts.VideoW, s.opts.VideoH)
		if err != nil {
			slog.Warn("ai: inference error", "error", err)
			s.storeSnapshot(nil)
			continue
		}
		kept := make([]Detection, 0, len(detections))
		for _, d := range detections {
			if d.Confidence >= s.opts.ConfidenceThreshold {
				kept = append(kept, d)
			}
		}
		s.inference.Add(1)
		s.storeSnapshot(kept)

		select {
		case s.events <- Event{CameraID: "0", Detections: kept, FrameNumber: frameNo}:
		default: // no SSE consumers / slow — drop, snapshots remain fresh
		}
	}
	slog.Info("ai: decoder stopped, service loop exiting")
}

func (s *Service) storeSnapshot(detections []Detection) {
	if detections == nil {
		detections = []Detection{}
	}
	s.mu.Lock()
	s.snap = Snapshot{
		Detections: detections,
		Model:      s.modelID,
		Timestamp:  time.Now().Unix(),
	}
	s.mu.Unlock()
}
