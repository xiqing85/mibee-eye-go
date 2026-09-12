package web

// SPEC v1 conformance tests for the web layer (contract:
// ../mibee-webui/SPEC.md). Covers the session+CSRF auth flow, the response
// envelope, the camera resource model, config GET/PUT semantics and the
// imaging extension paths.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newSpecServer builds a Server with the shared mock OnVIF config and the
// given web credentials. Empty credentials fall back to the mock's ONVIF
// ones (dialect A2); use newFirstBootServer for the unconfigured state.
func newFirstBootServer() *Server {
	return New(Config{
		Port:        8088,
		OnvifConfig: &mockOnvifConfig{port: 8080},
		Version:     "test",
	})
}
func newSpecServer(username, password string) *Server {
	return New(Config{
		Port:        8088,
		Username:    username,
		Password:    password,
		OnvifConfig: &mockOnvifConfig{port: 8080, username: "onvif-user", password: "onvif-pass"},
		Version:     "test",
	})
}

// doReq runs one request through the full mux (routes + auth wrappers).
func doReq(t *testing.T, s *Server, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	s.mux = http.NewServeMux()
	s.registerRoutes()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, reader)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON response: %v — body: %s", err, rec.Body.String())
	}
	return out
}

// login helper: returns the Cookie header value + csrf token.
func specLogin(t *testing.T, s *Server) (string, string) {
	t.Helper()
	rec := doReq(t, s, http.MethodPost, "/api/auth/login",
		`{"username":"admin","password":"spec-pass-1"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	var session, csrf string
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case "session":
			session = c.Value
		case "csrf-token":
			csrf = c.Value
		}
	}
	if session == "" || csrf == "" {
		t.Fatal("login must issue session + csrf cookies")
	}
	return "session=" + session, csrf
}

func authHdr(cookie, csrf string) map[string]string {
	return map[string]string{"Cookie": cookie, "X-CSRF-Token": csrf}
}

// ── session store ─────────────────────────────────────────────────────

func TestSessionStoreLifecycle(t *testing.T) {
	store := NewSessionStore()
	token, csrf, err := store.Create("admin")
	if err != nil {
		t.Fatal(err)
	}
	if user, c, err := store.Validate(token); err != nil || user != "admin" || c != csrf {
		t.Fatalf("validate: %v", err)
	}
	store.Logout(token)
	if _, _, err := store.Validate(token); err == nil {
		t.Fatal("logout must invalidate")
	}
	_, _, _ = store.Create("a")
	_, _, _ = store.Create("b")
	store.Clear()
	if store.Count() != 0 {
		t.Fatal("clear must empty the store")
	}
}

// ── auth state machine (SPEC §2) ──────────────────────────────────────

func TestMeFirstBootIsSetupRequired(t *testing.T) {
	s := newFirstBootServer()
	rec := doReq(t, s, http.MethodGet, "/api/auth/me", "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	body := decode(t, rec)
	if body["error"] != "setup_required" {
		t.Fatalf("want setup_required, got %v", body)
	}
}

func TestSetupCreatesAdminAndSession(t *testing.T) {
	s := newFirstBootServer()
	rec := doReq(t, s, http.MethodPost, "/api/auth/setup",
		`{"username":"admin","password":"longenough1"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body.String())
	}
	if len(rec.Result().Cookies()) < 2 {
		t.Fatal("setup must issue cookies")
	}
	// A second setup is rejected.
	rec = doReq(t, s, http.MethodPost, "/api/auth/setup",
		`{"username":"x","password":"longenough2"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("second setup must 400, got %d", rec.Code)
	}
	// Login now works with the new credentials.
	rec = doReq(t, s, http.MethodPost, "/api/auth/login",
		`{"username":"admin","password":"longenough1"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login after setup: %d", rec.Code)
	}
}

func TestSetupRejectsShortPassword(t *testing.T) {
	s := newFirstBootServer()
	rec := doReq(t, s, http.MethodPost, "/api/auth/setup",
		`{"username":"admin","password":"short"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

func TestLoginWrongPasswordIs401(t *testing.T) {
	s := newSpecServer("admin", "spec-pass-1")
	rec := doReq(t, s, http.MethodPost, "/api/auth/login",
		`{"username":"admin","password":"nope-nope"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	body := decode(t, rec)
	if body["error"] != "unauthorized" {
		t.Fatalf("envelope error code: %v", body)
	}
}

func TestLoginRateLimitBlocks(t *testing.T) {
	s := newSpecServer("admin", "spec-pass-1")
	for i := 0; i < 10; i++ {
		rec := doReq(t, s, http.MethodPost, "/api/auth/login",
			`{"username":"admin","password":"wrong"}`, nil)
		if rec.Code == http.StatusTooManyRequests {
			return // engaged
		}
	}
	rec := doReq(t, s, http.MethodPost, "/api/auth/login",
		`{"username":"admin","password":"wrong"}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit must engage, got %d", rec.Code)
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	s := newSpecServer("admin", "spec-pass-1")
	cookie, _ := specLogin(t, s)
	rec := doReq(t, s, http.MethodPost, "/api/auth/logout", "", map[string]string{"Cookie": cookie})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", rec.Code)
	}
	rec = doReq(t, s, http.MethodGet, "/api/config", "", map[string]string{"Cookie": cookie})
	if rec.Code != http.StatusUnauthorized {
		t.Fatal("session must die after logout")
	}
}

// ── auth gate + CSRF ──────────────────────────────────────────────────

func TestGatedReadsNeedSession(t *testing.T) {
	s := newSpecServer("admin", "spec-pass-1")
	if rec := doReq(t, s, http.MethodGet, "/api/config", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-cookie read must 401, got %d", rec.Code)
	}
	cookie, _ := specLogin(t, s)
	if rec := doReq(t, s, http.MethodGet, "/api/config", "", map[string]string{"Cookie": cookie}); rec.Code != http.StatusOK {
		t.Fatalf("authed read must 200, got %d", rec.Code)
	}
}

func TestCSRFRequiredOnWrites(t *testing.T) {
	s := newSpecServer("admin", "spec-pass-1")
	cookie, _ := specLogin(t, s)
	// PUT config without the CSRF header → 401.
	rec := doReq(t, s, http.MethodPut, "/api/config", `{"logging":{"level":"debug"}}`,
		map[string]string{"Cookie": cookie, "Content-Type": "application/json"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("write without CSRF must 401, got %d", rec.Code)
	}
	body := decode(t, rec)
	if body["error"] != "unauthorized" || !strings.Contains(rec.Body.String(), "csrf") {
		t.Fatalf("expected csrf mismatch, got %v", body)
	}
}

// ── envelope + core endpoints (SPEC §0, §1, §3, §4) ──────────────────

func TestHealthEnvelope(t *testing.T) {
	s := newSpecServer("", "")
	rec := doReq(t, s, http.MethodGet, "/api/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	body := decode(t, rec)
	if body["ok"] != true {
		t.Fatal("ok must be true")
	}
	data := body["data"].(map[string]interface{})
	if data["status"] != "ok" {
		t.Fatal("status must be ok")
	}
	if _, ok := data["uptime"].(float64); !ok {
		t.Fatal("uptime must be numeric")
	}
}

func TestStatusAndCapabilitiesShape(t *testing.T) {
	s := newSpecServer("admin", "spec-pass-1")
	cookie, _ := specLogin(t, s)

	rec := doReq(t, s, http.MethodGet, "/api/status", "", map[string]string{"Cookie": cookie})
	body := decode(t, rec)["data"].(map[string]interface{})
	for _, k := range []string{"device_name", "model", "vendor", "firmware", "uptime"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("status missing %s", k)
		}
	}

	rec = doReq(t, s, http.MethodGet, "/api/capabilities", "", map[string]string{"Cookie": cookie})
	caps := decode(t, rec)["data"].(map[string]interface{})
	if caps["spec_version"] != "1" {
		t.Fatal("spec_version must be 1")
	}
	if caps["multi_camera"] != false {
		t.Fatal("single camera device")
	}
	if caps["imaging"] != false { // no ParamManager in this server
		t.Fatal("imaging follows Params availability")
	}
	if caps["mjpeg"] != false {
		t.Fatal("Go dialect: no MJPEG")
	}
	// The frontend keys the save→restart UX on this flag (SPEC §3.1):
	// saving restart-sections self-restarts the service, so the UI waits
	// for recovery instead of offering a manual restart entry.
	if ca, ok := caps["config_apply"].(map[string]interface{}); !ok || ca["auto"] != true {
		t.Fatalf("config_apply.auto must be true on the Go dialect, got %v", caps["config_apply"])
	}
	if caps["mse"] != false { // no AUHub wired here
		t.Fatal("mse follows AUHub availability")
	}
}

func TestCamerasResource(t *testing.T) {
	s := newSpecServer("admin", "spec-pass-1")
	cookie, _ := specLogin(t, s)

	rec := doReq(t, s, http.MethodGet, "/api/cameras", "", map[string]string{"Cookie": cookie})
	arr := decode(t, rec)["data"].([]interface{})
	if len(arr) != 1 {
		t.Fatalf("want single camera, got %d", len(arr))
	}
	cam := arr[0].(map[string]interface{})
	if cam["id"] != "0" || cam["camera_type"] != "csi" {
		t.Fatalf("camera doc: %v", cam)
	}

	// Wrong id → 404 with the machine code.
	rec = doReq(t, s, http.MethodGet, "/api/cameras/9", "", map[string]string{"Cookie": cookie})
	if rec.Code != http.StatusNotFound {
		t.Fatal("want 404")
	}
	if decode(t, rec)["error"] != "not_found" {
		t.Fatal("machine code not_found")
	}
}

func TestStaticServing(t *testing.T) {
	s := newFirstBootServer()
	rec := doReq(t, s, http.MethodGet, "/", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "js/main.js") {
		t.Fatalf("index must reference the module entry: %d", rec.Code)
	}
	rec = doReq(t, s, http.MethodGet, "/js/main.js", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("module must be served: %d", rec.Code)
	}
}

// ── imaging extension paths ───────────────────────────────────────────

func TestImagingWrongCameraIs404(t *testing.T) {
	s := newSpecServer("admin", "spec-pass-1")
	cookie, csrf := specLogin(t, s)
	hdr := authHdr(cookie, csrf)
	rec := doReq(t, s, http.MethodGet, "/api/cameras/7/imaging/params", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

// ── SSE handler ───────────────────────────────────────────────────────

func TestEventsStreamRetryAndKeepalive(t *testing.T) {
	s := newSpecServer("admin", "spec-pass-1")
	s.mux = http.NewServeMux()
	s.registerRoutes()
	s.hub = newSSEHub(s.logger)

	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	// Login over the real server to get a cookie.
	resp, err := srv.Client().Post(srv.URL+"/api/auth/login",
		"application/json", strings.NewReader(`{"username":"admin","password":"spec-pass-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	var cookieHdr string
	for _, c := range resp.Cookies() {
		cookieHdr += c.Name + "=" + c.Value + "; "
	}
	resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	req.Header.Set("Cookie", cookieHdr)
	stream, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if ct := stream.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type: %s", ct)
	}

	buf := make([]byte, 64)
	n, _ := stream.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "retry: 3000") {
		t.Fatalf("expected leading retry frame, got %q", string(buf[:n]))
	}
}

// ── fMP4 muxer ────────────────────────────────────────────────────────

func TestFmp4Segments(t *testing.T) {
	sps := []byte{0x67, 0x42, 0x00, 0x1e, 0x00}
	pps := []byte{0x68, 0xCE, 0x38, 0x80}
	init := buildInitSegment(sps, pps, 1280, 720)
	for _, box := range []string{"ftyp", "moov", "avcC"} {
		if !bytes.Contains(init, []byte(box)) {
			t.Fatalf("init missing %s", box)
		}
	}
	seg := buildMediaSegment([][]byte{{0x65, 0x88, 0x84, 0x00}}, 1, 0, 6000, true)
	for _, box := range []string{"moof", "mdat"} {
		if !bytes.Contains(seg, []byte(box)) {
			t.Fatalf("segment missing %s", box)
		}
	}
}

// ── system restart (SPEC §5.1) ────────────────────────────────────────

// POST /api/system/restart responds immediately and fires the injected
// self-restart action (the real one SIGTERMs the process; systemd brings it
// back). Capabilities must advertise the restart extension.
func TestSystemRestartEndpoint(t *testing.T) {
	s := New(Config{Username: "admin", Password: "spec-pass-1"})
	var fired atomic.Bool
	s.selfRestart = func() { fired.Store(true) }

	cookie, csrf := specLogin(t, s)
	rec := doReq(t, s, http.MethodPost, "/api/system/restart", "", authHdr(cookie, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("restart failed: %d %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec)
	data, ok := out["data"].(map[string]interface{})
	if !ok || data["status"] != "restarting" {
		t.Fatalf("unexpected body: %v", out)
	}
	if !fired.Load() {
		t.Fatal("self-restart action must fire")
	}

	// Capability negotiation drives the UI's restart button.
	rec = doReq(t, s, http.MethodGet, "/api/capabilities", "", authHdr(cookie, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("capabilities failed: %d", rec.Code)
	}
	caps := decode(t, rec)["data"].(map[string]interface{})
	if caps["restart"] != true {
		t.Fatalf("capabilities.restart must be true, got %v", caps["restart"])
	}
}
