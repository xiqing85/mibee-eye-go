// Package metrics provides a Prometheus-compatible metrics exporter without
// third-party dependencies. It implements the Prometheus text exposition format
// (https://prometheus.io/docs/instrumenting/exposition_formats/) over HTTP.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
)

// Collector holds thread-safe Prometheus counters and gauges for MiBee Eye.
// All exported methods are safe for concurrent use.
type Collector struct {
	mu sync.Mutex

	framesCaptured uint64
	framesDropped  uint64
	rtspClients    int64
	cameraAlive    int64 // 1 if camera subprocess is alive, 0 otherwise
	onvifRequests  map[string]uint64
	aiInferences   uint64
}

// NewCollector creates a new metrics collector with initialized maps.
func NewCollector() *Collector {
	return &Collector{
		onvifRequests: make(map[string]uint64),
	}
}

// IncFramesCaptured increments the total frames captured counter by one.
func (c *Collector) IncFramesCaptured() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.framesCaptured++
}

// SetFramesCaptured sets the total frames captured counter to an absolute value.
// This is useful when pulling from an external source rather than incrementing locally.
func (c *Collector) SetFramesCaptured(n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.framesCaptured = n
}

// IncFramesDropped increments the total frames dropped counter by one.
func (c *Collector) IncFramesDropped() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.framesDropped++
}

// SetFramesDropped sets the total frames dropped counter to an absolute value.
func (c *Collector) SetFramesDropped(n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.framesDropped = n
}

// SetRTSPClients sets the current number of connected RTSP clients (gauge).
func (c *Collector) SetRTSPClients(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rtspClients = int64(n)
}

// SetCameraAlive sets whether the camera subprocess is alive (gauge: 1=alive, 0=dead).
func (c *Collector) SetCameraAlive(alive bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if alive {
		c.cameraAlive = 1
	} else {
		c.cameraAlive = 0
	}
}

// IncONVIFRequest increments the request counter for the given ONVIF action.
func (c *Collector) IncONVIFRequest(action string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onvifRequests[action]++
}

// SetAIInferences sets the completed AI detection inferences counter to an
// absolute value (pulled from the AI service on each poll).
func (c *Collector) SetAIInferences(n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.aiInferences = n
}

// ServeHTTP implements http.Handler and writes all metrics in Prometheus text
// exposition format (Content-Type: text/plain; version=0.0.4).
func (c *Collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	// Snapshot all values under the lock
	framesCaptured := c.framesCaptured
	framesDropped := c.framesDropped
	rtspClients := c.rtspClients
	cameraAlive := c.cameraAlive
	aiInferences := c.aiInferences

	// Copy and sort ONVIF request map for deterministic output
	actions := make([]string, 0, len(c.onvifRequests))
	actionCounts := make(map[string]uint64, len(c.onvifRequests))
	for action, count := range c.onvifRequests {
		actions = append(actions, action)
		actionCounts[action] = count
	}
	c.mu.Unlock()

	sort.Strings(actions)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	// Write frames captured counter
	fmt.Fprint(w, "# HELP mibee_eye_frames_captured_total Total frames captured\n")
	fmt.Fprint(w, "# TYPE mibee_eye_frames_captured_total counter\n")
	fmt.Fprintf(w, "mibee_eye_frames_captured_total %d\n", framesCaptured)

	// Write frames dropped counter
	fmt.Fprint(w, "# HELP mibee_eye_frames_dropped_total Total frames dropped due to slow consumers\n")
	fmt.Fprint(w, "# TYPE mibee_eye_frames_dropped_total counter\n")
	fmt.Fprintf(w, "mibee_eye_frames_dropped_total %d\n", framesDropped)

	// Write RTSP clients gauge
	fmt.Fprint(w, "# HELP mibee_eye_rtsp_clients Current number of connected RTSP clients\n")
	fmt.Fprint(w, "# TYPE mibee_eye_rtsp_clients gauge\n")
	fmt.Fprintf(w, "mibee_eye_rtsp_clients %d\n", rtspClients)

	// Write camera subprocess alive gauge
	fmt.Fprint(w, "# HELP mibee_eye_camera_subprocess_alive Camera subprocess alive status (1=alive, 0=dead)\n")
	fmt.Fprint(w, "# TYPE mibee_eye_camera_subprocess_alive gauge\n")
	fmt.Fprintf(w, "mibee_eye_camera_subprocess_alive %d\n", cameraAlive)

	// Write ONVIF requests counter by action
	fmt.Fprint(w, "# HELP mibee_eye_onvif_requests_total Total ONVIF SOAP requests by action\n")
	fmt.Fprint(w, "# TYPE mibee_eye_onvif_requests_total counter\n")
	for _, action := range actions {
		fmt.Fprintf(w, "mibee_eye_onvif_requests_total{action=%q} %d\n", action, actionCounts[action])
	}

	// Write AI inferences counter
	fmt.Fprint(w, "# HELP mibee_eye_ai_inferences_total Total AI detection inferences completed\n")
	fmt.Fprint(w, "# TYPE mibee_eye_ai_inferences_total counter\n")
	fmt.Fprintf(w, "mibee_eye_ai_inferences_total %d\n", aiInferences)
}

// Reset clears all counters and gauges back to zero. Useful for testing.
func (c *Collector) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.framesCaptured = 0
	c.framesDropped = 0
	c.rtspClients = 0
	c.cameraAlive = 0
	c.onvifRequests = make(map[string]uint64)
	c.aiInferences = 0
}
