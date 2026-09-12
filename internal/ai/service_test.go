package ai

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeDetector struct {
	model  string
	calls  int
	fail   bool
}

func (f *fakeDetector) Detect(frame *Frame, videoW, videoH uint32) ([]Detection, error) {
	f.calls++
	if f.fail {
		return nil, errNotBuilt
	}
	return []Detection{
		{Label: "person", Confidence: 0.9, BBox: [4]uint32{10, 20, 30, 40}},
		{Label: "cat", Confidence: 0.10, BBox: [4]uint32{1, 2, 3, 4}}, // below threshold
	}, nil
}

func (f *fakeDetector) ModelName() string { return f.model }

func (f *fakeDetector) InputSize() int { return 416 }

func newSvc(t *testing.T, opts Options) *Service {
	t.Helper()
	svc := NewService(opts, nil, func(Options) (Detector, error) {
		return &fakeDetector{model: "fake.onnx"}, nil
	})
	if svc == nil {
		t.Fatal("service must be active with a working detector factory")
	}
	return svc
}

func TestServiceDisabledReturnsNil(t *testing.T) {
	if svc := NewService(Options{}, nil, func(Options) (Detector, error) {
		t.Fatal("factory must not run when disabled")
		return nil, nil
	}); svc != nil {
		t.Fatal("disabled service must be nil (fail-open)")
	}
}

func TestServiceDetectorFailureReturnsNil(t *testing.T) {
	if svc := NewService(Options{Enabled: true}, nil, func(Options) (Detector, error) {
		return nil, errNotBuilt
	}); svc != nil {
		t.Fatal("broken detector must disable the service, not fake it")
	}
}

func TestServiceNilMethodsAreSafe(t *testing.T) {
	var svc *Service
	if svc.Active() || svc.ModelName() != "" || svc.Inferences() != 0 {
		t.Fatal("nil service must report inactive")
	}
	if snap := svc.Snapshot(); snap.Model != "" || snap.Detections != nil {
		t.Fatal("nil service snapshot must be empty")
	}
}

func TestRunLoopUpdatesSnapshotAndEvent(t *testing.T) {
	svc := newSvc(t, Options{Enabled: true, IntervalMs: 1000})
	frames := make(chan Frame, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.runLoop(ctx, frames)

	frames <- Frame{Width: 2, Height: 2, Data: make([]byte, 12)}
	evt := expectEvent(t, svc)
	if evt.CameraID != "0" || len(evt.Detections) != 1 {
		t.Fatalf("event = %+v", evt)
	}
	if evt.Detections[0].Label != "person" {
		t.Fatalf("label = %q", evt.Detections[0].Label)
	}

	snap := svc.Snapshot()
	// SPEC §4.6: the snapshot's model is the registry id, not the file name.
	if snap.Model != "nanodet-plus-m-320" || len(snap.Detections) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Timestamp == 0 {
		t.Fatal("timestamp must be set")
	}
	if svc.Inferences() != 1 {
		t.Fatalf("inferences = %d", svc.Inferences())
	}
}

func TestRunLoopIntervalGating(t *testing.T) {
	svc := newSvc(t, Options{Enabled: true, IntervalMs: 60_000})
	frames := make(chan Frame, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.runLoop(ctx, frames)

	// Two frames within the 60s window: only the first may infer.
	frames <- Frame{Width: 2, Height: 2, Data: make([]byte, 12)}
	expectEvent(t, svc)
	frames <- Frame{Width: 2, Height: 2, Data: make([]byte, 12)}
	select {
	case evt := <-svc.Events():
		t.Fatalf("second frame must be gated, got %+v", evt)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestRunLoopInferenceErrorClearsSnapshot(t *testing.T) {
	svc := NewService(Options{Enabled: true, IntervalMs: 1000}, nil, func(Options) (Detector, error) {
		return &fakeDetector{model: "failing.onnx", fail: true}, nil
	})
	if svc == nil {
		t.Fatal("service must be active")
	}
	frames := make(chan Frame, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.runLoop(ctx, frames)

	svc.mu.Lock()
	svc.snap = Snapshot{Detections: []Detection{{Label: "stale"}}, Model: "failing.onnx"}
	svc.mu.Unlock()

	frames <- Frame{Width: 2, Height: 2, Data: make([]byte, 12)}
	deadline := time.Now().Add(2 * time.Second)
	for svc.Snapshot().Timestamp == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(svc.Snapshot().Detections) != 0 {
		t.Fatalf("stale detections must be cleared, got %+v", svc.Snapshot().Detections)
	}
}

func expectEvent(t *testing.T, svc *Service) Event {
	t.Helper()
	select {
	case evt := <-svc.Events():
		return evt
	case <-time.After(2 * time.Second):
		t.Fatal("no ai_detection event within timeout")
		return Event{}
	}
}

// Wire-format guard: bboxes must serialize as JSON arrays [x,y,w,h]
// (SPEC §4.6) in both the snapshot and the SSE payload.
func TestDetectionWireFormat(t *testing.T) {
	snapJSON := mustJSON(t, Snapshot{
		Detections: []Detection{{Label: "chair", Confidence: 0.5, BBox: [4]uint32{1, 2, 3, 4}}},
		Model:      "m.onnx",
		Timestamp:  7,
	})
	for _, want := range []string{`"bbox":[1,2,3,4]`, `"label":"chair"`, `"model":"m.onnx"`, `"timestamp":7`} {
		if !strings.Contains(snapJSON, want) {
			t.Errorf("snapshot JSON missing %s: %s", want, snapJSON)
		}
	}
	evtJSON := mustJSON(t, Event{CameraID: "0", Detections: []Detection{{Label: "tv", Confidence: 0.4, BBox: [4]uint32{5, 6, 7, 8}}}, FrameNumber: 3})
	for _, want := range []string{`"camera_id":"0"`, `"bbox":[5,6,7,8]`, `"frame_number":3`} {
		if !strings.Contains(evtJSON, want) {
			t.Errorf("event JSON missing %s: %s", want, evtJSON)
		}
	}
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// withTempModel416 points the 416 registry entry at a real temp file so
// activation succeeds without /var/lib on the workstation, restoring the
// original path afterwards.
func withTempModel416(t *testing.T) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "nanodet-m-416.onnx")
	if err := os.WriteFile(f, []byte("onnx"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries := RegistryList()
	var old string
	for i, m := range entries {
		if m.ID == "nanodet-plus-m-416" {
			old = m.Path
			entries[i].Path = f
		}
	}
	SetRegistryEntries(entries)
	t.Cleanup(func() {
		entries := RegistryList()
		for i, m := range entries {
			if m.ID == "nanodet-plus-m-416" {
				entries[i].Path = old
			}
		}
		SetRegistryEntries(entries)
	})
}

func TestActivateModelSwapsModelIDAndSnapshot(t *testing.T) {
	withTempModel416(t)
	svc := newSvc(t, Options{Enabled: true})
	if svc.ModelID() != "nanodet-plus-m-320" {
		t.Fatalf("startup model = %s", svc.ModelID())
	}
	if err := svc.ActivateModel("nanodet-plus-m-416"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if svc.ModelID() != "nanodet-plus-m-416" {
		t.Fatalf("active model = %s", svc.ModelID())
	}
	snap := svc.Snapshot()
	if snap.Model != "nanodet-plus-m-416" {
		t.Fatalf("snapshot model = %s", snap.Model)
	}
}

func TestActivateModelUnknownIsError(t *testing.T) {
	svc := newSvc(t, Options{Enabled: true})
	err := svc.ActivateModel("yolo-9000")
	if !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("err = %v, want ErrUnknownModel", err)
	}
	if svc.ModelID() != "nanodet-plus-m-320" {
		t.Fatalf("model must be unchanged, got %s", svc.ModelID())
	}
}

func TestActivateModelMissingFileIsUnavailable(t *testing.T) {
	svc := newSvc(t, Options{Enabled: true})
	// The real 416 path does not exist on the workstation.
	err := svc.ActivateModel("nanodet-plus-m-416")
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("err = %v, want ErrModelUnavailable", err)
	}
	if svc.ModelID() != "nanodet-plus-m-320" {
		t.Fatalf("model must be unchanged, got %s", svc.ModelID())
	}
}

func TestActivateModelLoadFailureRollsBack(t *testing.T) {
	withTempModel416(t)
	svc := NewService(Options{Enabled: true}, nil, func(o Options) (Detector, error) {
		if o.Model == "nanodet-plus-m-416" {
			return nil, errNotBuilt
		}
		return &fakeDetector{model: "fake.onnx"}, nil
	})
	if svc == nil {
		t.Fatal("service must start with the working 320 factory")
	}
	if err := svc.ActivateModel("nanodet-plus-m-416"); err == nil {
		t.Fatal("load failure must be an error")
	}
	if svc.ModelID() != "nanodet-plus-m-320" {
		t.Fatalf("rollback must keep the old model, got %s", svc.ModelID())
	}
}

func TestRunLoopUsesSwappedDetector(t *testing.T) {
	withTempModel416(t)
	first := &fakeDetector{model: "fake.onnx"}
	calls := 0
	svc := NewService(Options{Enabled: true, IntervalMs: 1}, nil, func(Options) (Detector, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return &fakeDetector{model: "fake-416.onnx"}, nil
	})
	if svc == nil {
		t.Fatal("service must be active")
	}
	ctx, cancel := context.WithCancel(context.Background())
	frames := make(chan Frame)
	go svc.runLoop(ctx, frames)
	defer cancel()

	frames <- Frame{Width: 4, Height: 4, Data: make([]byte, 48)}
	// The inference result lands on the events channel — the happens-before
	// edge that makes reading loop state race-free.
	select {
	case <-svc.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("loop must run the first detector")
	}

	if err := svc.ActivateModel("nanodet-plus-m-416"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if svc.currentDetector() == first {
		t.Fatal("detector slot must hold a new instance after activation")
	}
}

type closeableDetector struct {
	fakeDetector
	closed bool
}

func (c *closeableDetector) Close() { c.closed = true }

func TestActivateModelClosesReplacedDetector(t *testing.T) {
	withTempModel416(t)
	first := &closeableDetector{fakeDetector: fakeDetector{model: "fake.onnx"}}
	svc := NewService(Options{Enabled: true}, nil, func(Options) (Detector, error) {
		return first, nil
	})
	if svc == nil {
		t.Fatal("service must be active")
	}
	if first.closed {
		t.Fatal("detector must not be closed before the swap")
	}
	if err := svc.ActivateModel("nanodet-plus-m-416"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if !first.closed {
		t.Fatal("replaced detector must be closed on hot-swap")
	}
}
