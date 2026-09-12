package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCollectorIncrement(t *testing.T) {
	c := NewCollector()
	c.IncFramesCaptured()
	c.IncFramesCaptured()
	c.IncFramesDropped()

	c.mu.Lock()
	if c.framesCaptured != 2 {
		t.Errorf("framesCaptured = %d, want 2", c.framesCaptured)
	}
	if c.framesDropped != 1 {
		t.Errorf("framesDropped = %d, want 1", c.framesDropped)
	}
	c.mu.Unlock()
}

func TestCollectorSet(t *testing.T) {
	c := NewCollector()
	c.SetFramesCaptured(100)
	c.SetFramesDropped(50)
	c.SetRTSPClients(3)
	c.SetCameraAlive(true)

	c.mu.Lock()
	if c.framesCaptured != 100 {
		t.Errorf("framesCaptured = %d, want 100", c.framesCaptured)
	}
	if c.framesDropped != 50 {
		t.Errorf("framesDropped = %d, want 50", c.framesDropped)
	}
	if c.rtspClients != 3 {
		t.Errorf("rtspClients = %d, want 3", c.rtspClients)
	}
	if c.cameraAlive != 1 {
		t.Errorf("cameraAlive = %d, want 1", c.cameraAlive)
	}
	c.mu.Unlock()

	// Test camera dead
	c.SetCameraAlive(false)
	c.mu.Lock()
	if c.cameraAlive != 0 {
		t.Errorf("cameraAlive = %d, want 0", c.cameraAlive)
	}
	c.mu.Unlock()
}

func TestCollectorONVIFRequests(t *testing.T) {
	c := NewCollector()
	c.IncONVIFRequest("GetProfiles")
	c.IncONVIFRequest("GetStreamUri")
	c.IncONVIFRequest("GetProfiles")

	c.mu.Lock()
	if c.onvifRequests["GetProfiles"] != 2 {
		t.Errorf("onvifRequests[GetProfiles] = %d, want 2", c.onvifRequests["GetProfiles"])
	}
	if c.onvifRequests["GetStreamUri"] != 1 {
		t.Errorf("onvifRequests[GetStreamUri] = %d, want 1", c.onvifRequests["GetStreamUri"])
	}
	c.mu.Unlock()
}

func TestServeHTTP(t *testing.T) {
	c := NewCollector()
	c.SetFramesCaptured(42)
	c.SetFramesDropped(7)
	c.SetRTSPClients(2)
	c.SetCameraAlive(true)
	c.IncONVIFRequest("GetProfiles")
	c.IncONVIFRequest("GetProfiles")
	c.IncONVIFRequest("GetStreamUri")

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	c.ServeHTTP(w, req)

	resp := w.Body.String()

	// Verify required metrics are present
	checks := []string{
		"# HELP mibee_eye_frames_captured_total",
		"# TYPE mibee_eye_frames_captured_total counter",
		"mibee_eye_frames_captured_total 42",
		"# HELP mibee_eye_frames_dropped_total",
		"# TYPE mibee_eye_frames_dropped_total counter",
		"mibee_eye_frames_dropped_total 7",
		"# HELP mibee_eye_rtsp_clients",
		"# TYPE mibee_eye_rtsp_clients gauge",
		"mibee_eye_rtsp_clients 2",
		"# HELP mibee_eye_camera_subprocess_alive",
		"# TYPE mibee_eye_camera_subprocess_alive gauge",
		"mibee_eye_camera_subprocess_alive 1",
		"# HELP mibee_eye_onvif_requests_total",
		"# TYPE mibee_eye_onvif_requests_total counter",
		`mibee_eye_onvif_requests_total{action="GetProfiles"} 2`,
		`mibee_eye_onvif_requests_total{action="GetStreamUri"} 1`,
	}

	for _, check := range checks {
		if !strings.Contains(resp, check) {
			t.Errorf("expected metric line not found: %s", check)
		}
	}
}

func TestServeHTTP_ContentType(t *testing.T) {
	c := NewCollector()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	c.ServeHTTP(w, req)

	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

func TestReset(t *testing.T) {
	c := NewCollector()
	c.SetFramesCaptured(100)
	c.SetFramesDropped(50)
	c.SetRTSPClients(3)
	c.IncONVIFRequest("GetProfiles")
	c.Reset()

	c.mu.Lock()
	if c.framesCaptured != 0 {
		t.Errorf("framesCaptured = %d, want 0 after reset", c.framesCaptured)
	}
	if c.framesDropped != 0 {
		t.Errorf("framesDropped = %d, want 0 after reset", c.framesDropped)
	}
	if c.rtspClients != 0 {
		t.Errorf("rtspClients = %d, want 0 after reset", c.rtspClients)
	}
	if len(c.onvifRequests) != 0 {
		t.Errorf("onvifRequests len = %d, want 0 after reset", len(c.onvifRequests))
	}
	c.mu.Unlock()
}
