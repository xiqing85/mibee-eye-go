package camera

import (
	"context"
	"sync"
	"testing"
)

// mockCamera is a test double that records SetParam calls.
type mockCamera struct {
	mu     sync.Mutex
	params map[string]interface{}
}

func newMockCamera() *mockCamera {
	return &mockCamera{
		params: make(map[string]interface{}),
	}
}

func (m *mockCamera) Start(_ context.Context) error { return nil }
func (m *mockCamera) Stop() error               { return nil }
func (m *mockCamera) Frames() <-chan Frame       { return nil }

func (m *mockCamera) SetParam(name string, value interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.params[name] = value
	return nil
}

func (m *mockCamera) GetParam(name string) (interface{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.params[name]
	if !ok {
		return 0, nil
	}
	return v, nil
}

func (m *mockCamera) Info() CameraInfo {
	return CameraInfo{}
}

func (m *mockCamera) getParam(name string) interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.params[name]
}

func TestSetBrightness(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Set("Brightness", float32(0.5))
	if err != nil {
		t.Fatalf("Set Brightness failed: %v", err)
	}

	got := mock.getParam("brightness")
	if got != float32(0.5) {
		t.Errorf("expected brightness=0.5, got %v (%T)", got, got)
	}
}

func TestSetContrast(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Set("Contrast", float32(2.0))
	if err != nil {
		t.Fatalf("Set Contrast failed: %v", err)
	}

	got := mock.getParam("contrast")
	if got != float32(2.0) {
		t.Errorf("expected contrast=2.0, got %v", got)
	}
}

func TestSetSaturation(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Set("Saturation", float32(1.5))
	if err != nil {
		t.Fatalf("Set Saturation failed: %v", err)
	}

	got := mock.getParam("saturation")
	if got != float32(1.5) {
		t.Errorf("expected saturation=1.5, got %v", got)
	}
}

func TestSetSharpness(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Set("Sharpness", float32(0.8))
	if err != nil {
		t.Fatalf("Set Sharpness failed: %v", err)
	}

	got := mock.getParam("sharpness")
	if got != float32(0.8) {
		t.Errorf("expected sharpness=0.8, got %v", got)
	}
}

func TestOutOfRangeBrightness(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Set("Brightness", float32(2.0))
	if err == nil {
		t.Fatal("expected error for out-of-range Brightness")
	}
	if got := mock.getParam("brightness"); got != nil {
		t.Errorf("camera should not have been called, got %v", got)
	}
}

func TestOutOfRangeContrast(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Set("Contrast", float32(-1.0))
	if err == nil {
		t.Fatal("expected error for out-of-range Contrast")
	}
	if got := mock.getParam("contrast"); got != nil {
		t.Errorf("camera should not have been called, got %v", got)
	}
}

func TestUnknownParam(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Set("FooBar", float32(1.0))
	if err == nil {
		t.Fatal("expected error for unknown parameter")
	}
}

func TestValidateWithoutApply(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	// Validate should return nil without calling camera
	err := pm.Validate("Brightness", float32(0.5))
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}

	// Camera should not have been called
	if got := mock.getParam("brightness"); got != nil {
		t.Errorf("Validate should not call camera, got %v", got)
	}
}

func TestValidateOutOfRange(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Validate("Brightness", float32(2.0))
	if err == nil {
		t.Fatal("expected error for out-of-range validation")
	}
}

func TestConcurrentAccess(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = pm.Set("Brightness", float32(0.1))
			_, _ = pm.Get("Brightness")
		}()
	}
	wg.Wait()

	// If we get here without race detector panic, test passes.
	got := mock.getParam("brightness")
	if got == nil {
		t.Error("expected brightness to be set")
	}
}

func TestSetHFlip(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Set("HFlip", float64(1))
	if err != nil {
		t.Fatalf("Set HFlip failed: %v", err)
	}

	got := mock.getParam("hFlip")
	if got != float64(1) {
		t.Errorf("expected hFlip=1, got %v (%T)", got, got)
	}
}

func TestSetVFlipBool(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Set("VFlip", true)
	if err != nil {
		t.Fatalf("Set VFlip failed: %v", err)
	}

	got := mock.getParam("vFlip")
	if got != true {
		t.Errorf("expected vFlip=true, got %v (%T)", got, got)
	}
}

func TestSetAWBMode(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Set("AWBMode", "auto")
	if err != nil {
		t.Fatalf("Set AWBMode failed: %v", err)
	}

	got := mock.getParam("awbMode")
	if got != "auto" {
		t.Errorf("expected awbMode=auto, got %v (%T)", got, got)
	}
}

func TestSetExposureMode(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Set("ExposureMode", "sport")
	if err != nil {
		t.Fatalf("Set ExposureMode failed: %v", err)
	}

	got := mock.getParam("exposureMode")
	if got != "sport" {
		t.Errorf("expected exposureMode=sport, got %v (%T)", got, got)
	}
}

func TestOutOfRangeHFlip(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Set("HFlip", float64(2))
	if err == nil {
		t.Fatal("expected error for out-of-range HFlip")
	}
	if got := mock.getParam("hFlip"); got != nil {
		t.Errorf("camera should not have been called, got %v", got)
	}
}

func TestInvalidAWBMode(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Set("AWBMode", "invalid_mode")
	if err == nil {
		t.Fatal("expected error for invalid AWBMode")
	}
	if got := mock.getParam("awbMode"); got != nil {
		t.Errorf("camera should not have been called, got %v", got)
	}
}

func TestExposureModeWrongType(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Set("ExposureMode", float64(1))
	if err == nil {
		t.Fatal("expected error for ExposureMode with non-string value")
	}
}

func TestValidateHFlip(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Validate("HFlip", float64(1))
	if err != nil {
		t.Fatalf("Validate HFlip failed: %v", err)
	}

	// Camera should not have been called
	if got := mock.getParam("hFlip"); got != nil {
		t.Errorf("Validate should not call camera, got %v", got)
	}
}

func TestValidateHFlipBoolType(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Validate("HFlip", true)
	if err != nil {
		t.Fatalf("Validate HFlip(true) failed: %v", err)
	}

	err = pm.Validate("HFlip", false)
	if err != nil {
		t.Fatalf("Validate HFlip(false) failed: %v", err)
	}
}

func TestValidateAWBMode(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Validate("AWBMode", "daylight")
	if err != nil {
		t.Fatalf("Validate AWBMode failed: %v", err)
	}
}

func TestValidateAWBModeInvalid(t *testing.T) {
	mock := newMockCamera()
	pm := NewParamManager(mock)

	err := pm.Validate("AWBMode", "nonsense")
	if err == nil {
		t.Fatal("expected error for invalid AWBMode")
	}
}
