package web

// PUT /api/config semantics (SPEC §5): deep merge over the YAML file,
// masked-secret restore, section preservation, atomic write.

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiqing85/mibee-eye-go/internal/config"
	"gopkg.in/yaml.v3"
)

// multiSectionYAML is a complete, valid config: PUT now validates the
// merged document before persisting, so the fixture must satisfy
// Config.Validate() on its own.
const multiSectionYAML = `camera:
  device: /dev/video0
  mode: rpicamvid
  width: 1280
  height: 720
  fps: 15
  codec: h264
  bitrate: 2000000
  idr_period: 15
  frame_buffer_size: 30
  max_backoff: 30s
rtsp:
  port: 8554
  subscriber_buffer_size: 64
  write_queue_size: 128
onvif:
  port: 8080
  username: admin
  password: onvif-secret
gb28181:
  enabled: false
logging:
  level: info
`

func configServer(t *testing.T, initial string) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newSpecServer("admin", "spec-pass-1")
	s.cfg.ConfigPath = path
	return s, path
}

func TestPutConfigDeepMergesAndPreservesSections(t *testing.T) {
	s, path := configServer(t, multiSectionYAML)
	cookie, csrf := specLogin(t, s)

	// Partial update: change logging.level and onvif.username; the masked
	// onvif.password round-trips back to the stored secret.
	rec := doReq(t, s, http.MethodPut, "/api/config",
		`{"logging":{"level":"debug"},"onvif":{"username":"nvr","password":"****"}}`,
		authHdr(cookie, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT config: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "" {
		body := decode(t, rec)
		if body["data"].(map[string]interface{})["applied"] != "restart" {
			t.Fatalf("applied semantics: %v", body)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]interface{}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}

	logging := cfg["logging"].(map[string]interface{})
	if logging["level"] != "debug" {
		t.Fatalf("merged logging.level = %v", logging["level"])
	}
	onvif := cfg["onvif"].(map[string]interface{})
	if onvif["username"] != "nvr" {
		t.Fatalf("merged onvif.username = %v", onvif["username"])
	}
	if onvif["password"] != "onvif-secret" {
		t.Fatalf("masked password must round-trip, got %v", onvif["password"])
	}
	// Untouched sections survive.
	cameraSec := cfg["camera"].(map[string]interface{})
	if cameraSec["device"] != "/dev/video0" || cameraSec["fps"] != 15 {
		t.Fatalf("camera section must be preserved: %v", cameraSec)
	}
	if _, ok := cfg["gb28181"]; !ok {
		t.Fatal("gb28181 section must be preserved")
	}
}

func TestPutConfigWithoutConfigPathIs501(t *testing.T) {
	s := newSpecServer("admin", "spec-pass-1")
	s.cfg.ConfigPath = ""
	cookie, csrf := specLogin(t, s)
	rec := doReq(t, s, http.MethodPut, "/api/config",
		`{"logging":{"level":"debug"}}`, authHdr(cookie, csrf))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("want 501, got %d", rec.Code)
	}
}

func TestGetConfigMasksSecrets(t *testing.T) {
	s, _ := configServer(t, multiSectionYAML)
	cookie, _ := specLogin(t, s)
	rec := doReq(t, s, http.MethodGet, "/api/config", "", map[string]string{"Cookie": cookie})
	data := decode(t, rec)["data"].(map[string]interface{})
	if data["onvif"].(map[string]interface{})["password"] != "****" {
		t.Fatal("onvif password must be masked")
	}
	if data["web"].(map[string]interface{})["password"] != "****" {
		t.Fatal("web password must be masked")
	}
}

func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := atomicWrite(path, []byte("a: 1\n")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.yaml" {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

// SPEC appendix A #19 + the validate-before-persist contract: PUT must
// reject an invalid merged config with 400 and leave the file untouched —
// boot-time validation exits the process, so persisting garbage would
// brick the service in a systemd restart loop.
func TestPutConfigInvalidRotationRejected(t *testing.T) {
	s, path := configServer(t, multiSectionYAML)
	cookie, csrf := specLogin(t, s)

	rec := doReq(t, s, http.MethodPut, "/api/config",
		`{"camera":{"rotation":45}}`, authHdr(cookie, csrf))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT invalid rotation: got %d %s, want 400", rec.Code, rec.Body.String())
	}
	// File on disk unchanged.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "rotation") {
		t.Fatalf("rejected update must not persist, file now: %s", data)
	}
	// The service was not asked to restart either (swappable hook).
}

func TestPutConfigInvalidFpsRejected(t *testing.T) {
	// Same contract for a pre-existing key: fps must stay positive.
	s, path := configServer(t, multiSectionYAML)
	cookie, csrf := specLogin(t, s)

	rec := doReq(t, s, http.MethodPut, "/api/config",
		`{"camera":{"fps":0}}`, authHdr(cookie, csrf))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT fps 0: got %d %s, want 400", rec.Code, rec.Body.String())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "fps: 15") {
		t.Fatalf("original fps must be intact, file: %s", data)
	}
}

// SPEC §5 applied:"camera_restart" (2026-09-25 addition): geometry-
// preserving camera changes apply via the in-place camera restart when
// the hook is wired; anything that reshapes the stream keeps the full
// process restart.
func TestPutConfigGeometryPreservingAppliesInPlace(t *testing.T) {
	s, _ := configServer(t, multiSectionYAML)
	restarts, camRestarts := 0, 0
	s.selfRestart = func() { restarts++ }
	s.restartCamera = func() error { camRestarts++; return nil }
	cookie, csrf := specLogin(t, s)

	rec := doReq(t, s, http.MethodPut, "/api/config",
		`{"camera":{"rotation":180}}`, authHdr(cookie, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT rotation 180: %d %s", rec.Code, rec.Body.String())
	}
	if got := decode(t, rec)["data"].(map[string]interface{})["applied"]; got != "camera_restart" {
		t.Fatalf("applied = %v, want camera_restart", got)
	}
	if camRestarts != 1 || restarts != 0 {
		t.Fatalf("camRestarts=%d restarts=%d, want 1/0", camRestarts, restarts)
	}

	// From 180 to 90 the effective dims swap — cross-geometry, so even
	// with the hook wired the change takes the process restart.
	rec = doReq(t, s, http.MethodPut, "/api/config",
		`{"camera":{"rotation":90}}`, authHdr(cookie, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT rotation 90: %d %s", rec.Code, rec.Body.String())
	}
	if got := decode(t, rec)["data"].(map[string]interface{})["applied"]; got != "restart" {
		t.Fatalf("applied = %v, want restart (180→90 swaps dims)", got)
	}
	if restarts != 1 || camRestarts != 1 {
		t.Fatalf("restarts=%d camRestarts=%d, want 1/1", restarts, camRestarts)
	}

	// 90↔270 preserves geometry (both transpose to the same dims).
	camRestarts = 0
	rec = doReq(t, s, http.MethodPut, "/api/config",
		`{"camera":{"rotation":270}}`, authHdr(cookie, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT rotation 270: %d", rec.Code)
	}
	if got := decode(t, rec)["data"].(map[string]interface{})["applied"]; got != "camera_restart" {
		t.Fatalf("applied = %v, want camera_restart (90→270 same dims)", got)
	}
	if camRestarts != 1 {
		t.Fatalf("camRestarts = %d, want 1", camRestarts)
	}
}

func TestPutConfigCrossGeometryTakesProcessRestart(t *testing.T) {
	s, _ := configServer(t, strings.Replace(multiSectionYAML, "mode: rpicamvid", "mode: v4l2", 1))
	restarts, camRestarts := 0, 0
	s.selfRestart = func() { restarts++ }
	s.restartCamera = func() error { camRestarts++; return nil }
	cookie, csrf := specLogin(t, s)

	// 0 → 90 swaps effective dims → full restart.
	rec := doReq(t, s, http.MethodPut, "/api/config",
		`{"camera":{"rotation":90}}`, authHdr(cookie, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT rotation 90 (v4l2): %d %s", rec.Code, rec.Body.String())
	}
	if got := decode(t, rec)["data"].(map[string]interface{})["applied"]; got != "restart" {
		t.Fatalf("applied = %v, want restart", got)
	}
	if restarts != 1 || camRestarts != 0 {
		t.Fatalf("restarts=%d camRestarts=%d, want 1/0", restarts, camRestarts)
	}
}

func TestPutConfigFpsChangeTakesProcessRestart(t *testing.T) {
	s, _ := configServer(t, multiSectionYAML)
	restarts, camRestarts := 0, 0
	s.selfRestart = func() { restarts++ }
	s.restartCamera = func() error { camRestarts++; return nil }
	cookie, csrf := specLogin(t, s)

	rec := doReq(t, s, http.MethodPut, "/api/config",
		`{"camera":{"fps":20}}`, authHdr(cookie, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT fps 20: %d", rec.Code)
	}
	if got := decode(t, rec)["data"].(map[string]interface{})["applied"]; got != "restart" {
		t.Fatalf("applied = %v, want restart (fps changes SPS VUI timing)", got)
	}
}

func TestPutConfigCameraRestartFailureFallsBackToProcessRestart(t *testing.T) {
	s, _ := configServer(t, multiSectionYAML)
	restarts := 0
	s.selfRestart = func() { restarts++ }
	s.restartCamera = func() error { return errors.New("device busy") }
	cookie, csrf := specLogin(t, s)

	rec := doReq(t, s, http.MethodPut, "/api/config",
		`{"camera":{"rotation":180}}`, authHdr(cookie, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d", rec.Code)
	}
	if got := decode(t, rec)["data"].(map[string]interface{})["applied"]; got != "restart" {
		t.Fatalf("applied = %v, want restart (fallback)", got)
	}
	if restarts != 1 {
		t.Fatalf("restarts = %d, want 1 (fallback fired)", restarts)
	}
}

func TestPutConfigNoHookKeepsLegacyRestart(t *testing.T) {
	s, _ := configServer(t, multiSectionYAML) // restartCamera == nil
	restarts := 0
	s.selfRestart = func() { restarts++ }
	cookie, csrf := specLogin(t, s)
	rec := doReq(t, s, http.MethodPut, "/api/config",
		`{"camera":{"rotation":180}}`, authHdr(cookie, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d", rec.Code)
	}
	if got := decode(t, rec)["data"].(map[string]interface{})["applied"]; got != "restart" {
		t.Fatalf("applied = %v, want restart without hook", got)
	}
}

func TestCameraRestartIneligibleOnSubstreamChange(t *testing.T) {
	old := config.DefaultConfig().Camera
	newCfg := old
	newCfg.Substream.Enabled = true
	if cameraRestartEligible(old, newCfg) {
		t.Fatal("substream changes need the full process restart (boot-static pipeline)")
	}
	if !cameraRestartEligible(old, old) {
		t.Fatal("identical config must stay eligible")
	}
}
