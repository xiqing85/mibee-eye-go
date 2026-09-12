package web

// Device-level flips via the imaging endpoint (SPEC §4.5 + 附录A #9, Go
// dialect): rpicam-vid has no runtime flip channel, so POST .../imaging/param
// with VFlip/HFlip must persist into the YAML camera section and restart the
// service — a response that claims success while only mutating in-memory
// state leaves the stream unchanged and the setting lost on restart.
// GET /api/config must NOT overlay live PascalCase params into the camera
// section: the editor round-trip would write them back as duplicate keys.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/camera"

	"gopkg.in/yaml.v3"
)

// fakeCamera is a camera.Camera stand-in that records SetParam calls.
type fakeCamera struct {
	params map[string]interface{}
}

func newFakeCamera() *fakeCamera {
	return &fakeCamera{params: map[string]interface{}{}}
}

func (f *fakeCamera) Start(ctx context.Context) error { return nil }
func (f *fakeCamera) Stop() error                     { return nil }
func (f *fakeCamera) Frames() <-chan camera.Frame     { return nil }
func (f *fakeCamera) SetParam(name string, v interface{}) error {
	f.params[name] = v
	return nil
}
func (f *fakeCamera) GetParam(name string) (interface{}, error) {
	if v, ok := f.params[name]; ok {
		return v, nil
	}
	return nil, nil
}
func (f *fakeCamera) Info() camera.CameraInfo { return camera.CameraInfo{} }

func flipServer(t *testing.T, initial string) (*Server, string, *atomic.Int32) {
	t.Helper()
	s, path := configServer(t, initial)
	s.cfg.Params = camera.NewParamManager(newFakeCamera())
	var restarts atomic.Int32
	s.selfRestart = func() { restarts.Add(1) }
	return s, path, &restarts
}

func yamlCameraSection(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]interface{}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	sec, ok := cfg["camera"].(map[string]interface{})
	if !ok {
		t.Fatalf("camera section missing: %v", cfg)
	}
	return sec
}

func postFlip(t *testing.T, s *Server, name string, value bool) *httptest.ResponseRecorder {
	t.Helper()
	cookie, csrf := specLogin(t, s)
	body, err := json.Marshal(map[string]interface{}{"name": name, "value": value})
	if err != nil {
		t.Fatal(err)
	}
	return doReq(t, s, http.MethodPost, "/api/cameras/0/imaging/param", string(body), authHdr(cookie, csrf))
}

// Flipping VFlip through the imaging endpoint persists camera.vflip into the
// YAML config and fires the self-restart (applied:"restart").
func TestImagingFlipPersistsToConfigAndRestarts(t *testing.T) {
	s, path, restarts := flipServer(t, multiSectionYAML)

	rec := postFlip(t, s, "VFlip", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("flip POST: %d %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec)["data"].(map[string]interface{})
	if out["applied"] != "restart" {
		t.Fatalf("flip must declare restart semantics, got %v", out)
	}
	if restarts.Load() != 1 {
		t.Fatalf("changed flip must restart exactly once, got %d", restarts.Load())
	}

	sec := yamlCameraSection(t, path)
	if sec["vflip"] != true {
		t.Fatalf("camera.vflip must be persisted as true, got %v", sec["vflip"])
	}
	if _, dup := sec["VFlip"]; dup {
		t.Fatalf("PascalCase VFlip must not leak into the config file: %v", sec)
	}
}

// HFlip behaves identically.
func TestImagingHFlipPersistsToConfig(t *testing.T) {
	s, path, restarts := flipServer(t, multiSectionYAML)

	rec := postFlip(t, s, "HFlip", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("flip POST: %d %s", rec.Code, rec.Body.String())
	}
	if restarts.Load() != 1 {
		t.Fatalf("changed flip must restart exactly once, got %d", restarts.Load())
	}
	sec := yamlCameraSection(t, path)
	if sec["hflip"] != true {
		t.Fatalf("camera.hflip must be persisted as true, got %v", sec["hflip"])
	}
}

// Toggling off writes false and stays persisted.
func TestImagingFlipToggleOff(t *testing.T) {
	s, path, _ := flipServer(t, multiSectionYAML)

	if rec := postFlip(t, s, "VFlip", true); rec.Code != http.StatusOK {
		t.Fatalf("flip on: %d %s", rec.Code, rec.Body.String())
	}
	if rec := postFlip(t, s, "VFlip", false); rec.Code != http.StatusOK {
		t.Fatalf("flip off: %d %s", rec.Code, rec.Body.String())
	}
	sec := yamlCameraSection(t, path)
	if sec["vflip"] != false {
		t.Fatalf("camera.vflip must round-trip to false, got %v", sec["vflip"])
	}
}

// A flip POST whose value already matches the live pipeline is an idempotent
// no-op: no restart, no "applied" marker, YAML untouched. resetDefaults
// POSTs both flips back-to-back with default values — when the device
// already sits at those defaults the requests must not stack restarts
// against the systemd StartLimit.
func TestImagingFlipUnchangedValueIsNoOp(t *testing.T) {
	s, path, restarts := flipServer(t, multiSectionYAML)

	if rec := postFlip(t, s, "VFlip", true); rec.Code != http.StatusOK {
		t.Fatalf("flip on: %d %s", rec.Code, rec.Body.String())
	}
	if restarts.Load() != 1 {
		t.Fatalf("changed flip must restart once, got %d", restarts.Load())
	}

	rec := postFlip(t, s, "VFlip", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("unchanged flip: %d %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec)["data"].(map[string]interface{})
	if _, ok := out["applied"]; ok {
		t.Fatalf("unchanged flip must not claim applied semantics, got %v", out)
	}
	if restarts.Load() != 1 {
		t.Fatalf("unchanged flip must not restart again, got %d restarts", restarts.Load())
	}
	if sec := yamlCameraSection(t, path); sec["vflip"] != true {
		t.Fatalf("camera.vflip must stay true, got %v", sec["vflip"])
	}
}

// GET /api/config camera section never contains live PascalCase params (the
// editor would write them back verbatim as duplicate keys); the persistent
// lowercase flip fields are always present.
func TestGetConfigCameraSectionHasNoPascalCaseOverlay(t *testing.T) {
	s, _, _ := flipServer(t, multiSectionYAML)
	cookie, _ := specLogin(t, s)

	rec := doReq(t, s, http.MethodGet, "/api/config", "", map[string]string{"Cookie": cookie})
	cam := decode(t, rec)["data"].(map[string]interface{})["camera"].(map[string]interface{})
	if _, ok := cam["vflip"]; !ok {
		t.Fatal("camera.vflip must be present in GET /api/config")
	}
	if _, ok := cam["hflip"]; !ok {
		t.Fatal("camera.hflip must be present in GET /api/config")
	}
	for _, ghost := range []string{"VFlip", "HFlip", "Brightness", "FPS", "Contrast"} {
		if _, ok := cam[ghost]; ok {
			t.Fatalf("GET /api/config camera section must not overlay live param %q", ghost)
		}
	}
}

// The imaging params endpoint (SPEC §4.5, the panel's data source) still
// serves the flip under its PascalCase name with the new value.
func TestImagingParamsReflectFlip(t *testing.T) {
	s, _, _ := flipServer(t, multiSectionYAML)
	cookie, csrf := specLogin(t, s)

	if rec := postFlip(t, s, "VFlip", true); rec.Code != http.StatusOK {
		t.Fatalf("flip POST: %d %s", rec.Code, rec.Body.String())
	}
	rec := doReq(t, s, http.MethodGet, "/api/cameras/0/imaging/params", "", authHdr(cookie, csrf))
	params := decode(t, rec)["data"].(map[string]interface{})
	if params["VFlip"] != true {
		t.Fatalf("imaging params VFlip must be true, got %v", params["VFlip"])
	}
}
