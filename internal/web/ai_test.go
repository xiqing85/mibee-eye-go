package web

import (
	"bytes"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/ai"
	"gopkg.in/yaml.v3"
)

func aiTestServer(t *testing.T, active bool) *Server {
	t.Helper()
	var svc *ai.Service
	if active {
		svc = ai.NewService(ai.Options{Enabled: true}, nil, func(ai.Options) (ai.Detector, error) {
			return &fakeAIDetector{}, nil
		})
		if svc == nil {
			t.Fatal("service must be active with a working detector")
		}
	}
	return New(Config{
		Port:        8088,
		Username:    "admin",
		Password:    "spec-pass-1",
		OnvifConfig: &mockOnvifConfig{port: 8080, username: "onvif-user", password: "onvif-pass"},
		Version:     "test",
		AI:          svc,
	})
}

type fakeAIDetector struct{}

func (fakeAIDetector) Detect(*ai.Frame, uint32, uint32) ([]ai.Detection, error) {
	return []ai.Detection{{Label: "chair", Confidence: 0.7, BBox: [4]uint32{8, 9, 10, 11}}}, nil
}

func (fakeAIDetector) ModelName() string { return "fake.onnx" }

func (fakeAIDetector) InputSize() int { return 416 }

func TestDetectionsDisabledReportsEnabledFalse(t *testing.T) {
	s := aiTestServer(t, false)
	cookie, _ := specLogin(t, s)
	rec := doReq(t, s, http.MethodGet, "/api/detections", "", map[string]string{"Cookie": cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decode(t, rec)
	data := body["data"].(map[string]interface{})
	if data["enabled"] != false {
		t.Fatalf("data = %v, want enabled:false", data)
	}
}

func TestCapabilitiesAIRelated(t *testing.T) {
	s := aiTestServer(t, false)
	cookie, _ := specLogin(t, s)
	rec := doReq(t, s, http.MethodGet, "/api/capabilities", "", map[string]string{"Cookie": cookie})
	data := decode(t, rec)["data"].(map[string]interface{})
	if data["ai"] != false {
		t.Fatalf("ai capability = %v, want false without service", data["ai"])
	}
	events := data["events"].([]interface{})
	for _, e := range events {
		if e == "ai_detection" {
			t.Fatal("ai_detection must not be advertised when AI is off")
		}
	}
}

func TestDetectionsRequiresAuth(t *testing.T) {
	s := aiTestServer(t, true)
	rec := doReq(t, s, http.MethodGet, "/api/detections", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}
}

func TestDetectionsActiveServesSnapshot(t *testing.T) {
	s := aiTestServer(t, true)
	cookie, _ := specLogin(t, s)
	rec := doReq(t, s, http.MethodGet, "/api/detections", "", map[string]string{"Cookie": cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	body := decode(t, rec)
	data := body["data"].(map[string]interface{})
	// SPEC §4.6: "model" is the registry id, not the detector file name.
	if data["model"] != "nanodet-plus-m-320" {
		t.Fatalf("model = %v", data["model"])
	}
	if _, ok := data["detections"]; !ok {
		t.Fatalf("detections key missing: %v", data)
	}
}

// withTempModel416 points the 416 registry entry at a real temp file so
// activation succeeds without /var/lib on the workstation.
func withTempModel416(t *testing.T) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "nanodet-m-416.onnx")
	if err := os.WriteFile(f, []byte("onnx"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Point the 416 entry at a temp file via the runtime registry.
	entries := ai.RegistryList()
	var old string
	for i, m := range entries {
		if m.ID == "nanodet-plus-m-416" {
			old = m.Path
			entries[i].Path = f
		}
	}
	ai.SetRegistryEntries(entries)
	t.Cleanup(func() {
		entries := ai.RegistryList()
		for i, m := range entries {
			if m.ID == "nanodet-plus-m-416" {
				entries[i].Path = old
			}
		}
		ai.SetRegistryEntries(entries)
	})
}

// aiModelTestServer is aiTestServer plus a real (temp) config file so the
// activate endpoint can exercise its persist step.
func aiModelTestServer(t *testing.T) *Server {
	t.Helper()
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	seed := "ai:\n  enabled: true\n  model: nanodet-plus-m-320\n"
	if err := os.WriteFile(cfgFile, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := ai.NewService(ai.Options{Enabled: true}, nil, func(ai.Options) (ai.Detector, error) {
		return &fakeAIDetector{}, nil
	})
	if svc == nil {
		t.Fatal("service must be active")
	}
	return New(Config{
		Port:        8088,
		Username:    "admin",
		Password:    "spec-pass-1",
		ConfigPath:  cfgFile,
		Version:     "test",
		AI:          svc,
	})
}

func TestAIModelsListsRegistry(t *testing.T) {
	s := aiTestServer(t, true)
	cookie, _ := specLogin(t, s)
	rec := doReq(t, s, http.MethodGet, "/api/ai/models", "", map[string]string{"Cookie": cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	data := decode(t, rec)["data"].(map[string]interface{})
	if data["active"] != "nanodet-plus-m-320" {
		t.Fatalf("active = %v", data["active"])
	}
	models := data["models"].([]interface{})
	ids := map[string]bool{}
	for _, m := range models {
		e := m.(map[string]interface{})
		ids[e["id"].(string)] = true
		if e["source"] != "builtin" {
			t.Fatalf("entry = %v", e)
		}
		// Families must be decoders this build actually implements.
		switch e["family"] {
		case "nanodet", "yolox":
		default:
			t.Fatalf("unknown family in entry = %v", e)
		}
	}
	if !ids["nanodet-plus-m-320"] || !ids["nanodet-plus-m-416"] || !ids["yolox-nano-416"] {
		t.Fatalf("registry ids missing: %v", ids)
	}
}

func TestAIModelsRequiresAuth(t *testing.T) {
	s := aiTestServer(t, true)
	rec := doReq(t, s, http.MethodGet, "/api/ai/models", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestActivateModelFlowPersistsAndReports(t *testing.T) {
	withTempModel416(t)
	s := aiModelTestServer(t)
	cookie, csrf := specLogin(t, s)

	rec := doReq(t, s, http.MethodPost, "/api/ai/models/nanodet-plus-m-416/activate", "",
		map[string]string{"Cookie": cookie, "X-CSRF-Token": csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	data := decode(t, rec)["data"].(map[string]interface{})
	if data["active"] != "nanodet-plus-m-416" || data["applied"] != "immediate" {
		t.Fatalf("data = %v", data)
	}

	// The service reports the new model id.
	if m := s.cfg.AI.ModelID(); m != "nanodet-plus-m-416" {
		t.Fatalf("ModelID = %s", m)
	}

	// The choice persisted to the YAML ai section.
	raw, err := os.ReadFile(s.cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]interface{}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	sec := cfg["ai"].(map[string]interface{})
	if sec["model"] != "nanodet-plus-m-416" {
		t.Fatalf("persisted model = %v", sec["model"])
	}
	if sec["model_path"] != ai.DefaultModelPath {
		t.Fatalf("persisted model_path = %v", sec["model_path"])
	}

	// /api/detections now carries the new model id.
	rec = doReq(t, s, http.MethodGet, "/api/detections", "", map[string]string{"Cookie": cookie})
	snap := decode(t, rec)["data"].(map[string]interface{})
	if snap["model"] != "nanodet-plus-m-416" {
		t.Fatalf("snapshot model = %v", snap["model"])
	}
}

func TestActivateModelUnknownIs404(t *testing.T) {
	s := aiModelTestServer(t)
	cookie, csrf := specLogin(t, s)
	rec := doReq(t, s, http.MethodPost, "/api/ai/models/yolo-9000/activate", "",
		map[string]string{"Cookie": cookie, "X-CSRF-Token": csrf})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if m := s.cfg.AI.ModelID(); m != "nanodet-plus-m-320" {
		t.Fatalf("model must be unchanged, got %s", m)
	}
}

func TestActivateModelUnavailableIs409(t *testing.T) {
	// The real 416 path does not exist on the workstation.
	s := aiModelTestServer(t)
	cookie, csrf := specLogin(t, s)
	rec := doReq(t, s, http.MethodPost, "/api/ai/models/nanodet-plus-m-416/activate", "",
		map[string]string{"Cookie": cookie, "X-CSRF-Token": csrf})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if m := s.cfg.AI.ModelID(); m != "nanodet-plus-m-320" {
		t.Fatalf("rollback must keep the old model, got %s", m)
	}
}

func TestCapabilitiesAnnounceAIModels(t *testing.T) {
	s := aiTestServer(t, true)
	cookie, _ := specLogin(t, s)
	rec := doReq(t, s, http.MethodGet, "/api/capabilities", "", map[string]string{"Cookie": cookie})
	caps := decode(t, rec)["data"].(map[string]interface{})
	if caps["ai_models"] != true {
		t.Fatalf("ai_models = %v", caps["ai_models"])
	}
	for _, e := range caps["events"].([]interface{}) {
		if e == "ai_model_changed" {
			return
		}
	}
	t.Fatal("events must announce ai_model_changed")

	// Disabled AI keeps the capability off.
	s2 := aiTestServer(t, false)
	cookie2, _ := specLogin(t, s2)
	rec2 := doReq(t, s2, http.MethodGet, "/api/capabilities", "", map[string]string{"Cookie": cookie2})
	caps2 := decode(t, rec2)["data"].(map[string]interface{})
	if caps2["ai_models"] != false {
		t.Fatalf("ai_models = %v", caps2["ai_models"])
	}
}

// aiUploadTestServer wires an active service, uploads enabled in a real
// (temp) config file, and a real (temp) models dir behind the registry.
func aiUploadTestServer(t *testing.T) *Server {
	t.Helper()
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("ai:\n  enabled: true\n  model: nanodet-plus-m-320\n  allow_upload: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	modelsDir := t.TempDir()
	ai.InitRegistry(modelsDir)
	svc := ai.NewService(ai.Options{Enabled: true}, nil, func(ai.Options) (ai.Detector, error) {
		return &fakeAIDetector{}, nil
	})
	if svc == nil {
		t.Fatal("service must be active")
	}
	return New(Config{
		Port: 8088, Username: "admin", Password: "spec-pass-1",
		ConfigPath: cfgFile, Version: "test", AI: svc,
	})
}

// doMultipartReq registers routes and runs a raw multipart request.
func doMultipartReq(t *testing.T, s *Server, method, path string, body []byte, cookie, csrf, ctype string) *httptest.ResponseRecorder {
	t.Helper()
	s.mux = http.NewServeMux()
	s.registerRoutes()
	req, _ := http.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Cookie", cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Content-Type", ctype)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func multipartAIUpload(t *testing.T, id, family string, content []byte) (string, []byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("family", family)
	fw, _ := mw.CreateFormFile("file", "model.onnx")
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	mw.Close()
	return "POST", buf.Bytes(), mw.FormDataContentType()
}

func TestUploadActivateDeleteRoundtrip(t *testing.T) {
	s := aiUploadTestServer(t)
	cookie, csrf := specLogin(t, s)

	_, body, ctype := multipartAIUpload(t, "my-model", "yolox", []byte("fake-onnx"))
	rec := doMultipartReq(t, s, "POST", "/api/ai/models/my-model", body, cookie, csrf, ctype)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d: %s", rec.Code, rec.Body.String())
	}

	// Listed as uploaded with the factory-reported input size.
	rec = doReq(t, s, http.MethodGet, "/api/ai/models", "", map[string]string{"Cookie": cookie})
	data := decode(t, rec)["data"].(map[string]interface{})
	var entry map[string]interface{}
	for _, m := range data["models"].([]interface{}) {
		if e := m.(map[string]interface{}); e["id"] == "my-model" {
			entry = e
		}
	}
	if entry == nil || entry["source"] != "uploaded" || entry["input"] != float64(416) {
		t.Fatalf("entry = %v", entry)
	}
	if _, ok := data["upload"]; !ok {
		t.Fatal("upload metadata missing")
	}

	// Activates; deleting the running model is refused; after switching
	// back, delete succeeds and removes the file.
	rec = doReq(t, s, http.MethodPost, "/api/ai/models/my-model/activate", "", map[string]string{"Cookie": cookie, "X-CSRF-Token": csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("activate = %d", rec.Code)
	}
	rec = doReq(t, s, http.MethodDelete, "/api/ai/models/my-model", "", map[string]string{"Cookie": cookie, "X-CSRF-Token": csrf})
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete active = %d", rec.Code)
	}
	// Switch to the 416 entry (temp-file swapped) so my-model stops
	// being the active model, then delete it.
	withTempModel416(t)
	rec = doReq(t, s, http.MethodPost, "/api/ai/models/nanodet-plus-m-416/activate", "", map[string]string{"Cookie": cookie, "X-CSRF-Token": csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("activate 416 = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, s, http.MethodDelete, "/api/ai/models/my-model", "", map[string]string{"Cookie": cookie, "X-CSRF-Token": csrf})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := ai.Find("my-model"); ok {
		t.Fatal("entry must be gone from the registry")
	}
}

func TestUploadRejectsBadRequests(t *testing.T) {
	s := aiUploadTestServer(t)
	cookie, csrf := specLogin(t, s)

	for _, tc := range []struct {
		id, family string
		want       int
	}{
		{"nanodet-plus-m-320", "nanodet", http.StatusConflict},
		{"Bad_ID!", "nanodet", http.StatusBadRequest},
		{"ok-id", "yolo9000", http.StatusBadRequest},
	} {
		_, body, ctype := multipartAIUpload(t, tc.id, tc.family, []byte("x"))
		rec := doMultipartReq(t, s, "POST", "/api/ai/models/"+tc.id, body, cookie, csrf, ctype)
		if rec.Code != tc.want {
			t.Fatalf("id=%s family=%s: status = %d, want %d", tc.id, tc.family, rec.Code, tc.want)
		}
	}

	// Builtin delete → 409; unknown → 404.
	rec := doReq(t, s, http.MethodDelete, "/api/ai/models/nanodet-plus-m-320", "", map[string]string{"Cookie": cookie, "X-CSRF-Token": csrf})
	if rec.Code != http.StatusConflict {
		t.Fatalf("builtin delete = %d", rec.Code)
	}
	rec = doReq(t, s, http.MethodDelete, "/api/ai/models/no-such", "", map[string]string{"Cookie": cookie, "X-CSRF-Token": csrf})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown delete = %d", rec.Code)
	}
}

func TestUploadDisabledAnswers501(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(cfgFile, []byte("ai:\n  enabled: true\n"), 0o600)
	ai.InitRegistry(t.TempDir())
	svc := ai.NewService(ai.Options{Enabled: true}, nil, func(ai.Options) (ai.Detector, error) {
		return &fakeAIDetector{}, nil
	})
	s := New(Config{Port: 8088, Username: "admin", Password: "spec-pass-1",
		ConfigPath: cfgFile, Version: "test", AI: svc})
	cookie, csrf := specLogin(t, s)
	_, body, ctype := multipartAIUpload(t, "some-model", "yolox", []byte("x"))
	rec := doMultipartReq(t, s, "POST", "/api/ai/models/some-model", body, cookie, csrf, ctype)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
}


func TestUploadValidationFailureLeavesNoTrace(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("ai:\n  enabled: true\n  allow_upload: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	modelsDir := t.TempDir()
	ai.InitRegistry(modelsDir)
	t.Cleanup(func() { ai.InitRegistry("") })
	svc := ai.NewService(ai.Options{Enabled: true}, nil, func(o ai.Options) (ai.Detector, error) {
		if o.Family == "yolox" {
			return nil, errors.New("corrupt graph")
		}
		return &fakeAIDetector{}, nil
	})
	s := New(Config{Port: 8088, Username: "admin", Password: "spec-pass-1",
		ConfigPath: cfgFile, Version: "test", AI: svc})
	cookie, csrf := specLogin(t, s)

	method, body, ctype := multipartAIUpload(t, "broken-model", "yolox", []byte("junk"))
	rec := doMultipartReq(t, s, method, "/api/ai/models/broken-model", body, cookie, csrf, ctype)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	entries, _ := os.ReadDir(modelsDir)
	if len(entries) != 0 {
		t.Fatalf("no files must remain after failed validation, got %d", len(entries))
	}
	if _, ok := ai.Find("broken-model"); ok {
		t.Fatal("failed model must not enter the registry")
	}
}
