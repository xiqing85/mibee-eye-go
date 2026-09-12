package web

// PUT /api/config semantics (SPEC §5): deep merge over the YAML file,
// masked-secret restore, section preservation, atomic write.

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

const multiSectionYAML = `camera:
  device: /dev/video0
  width: 1280
  height: 720
  fps: 15
  codec: h264
  bitrate: 2000000
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
