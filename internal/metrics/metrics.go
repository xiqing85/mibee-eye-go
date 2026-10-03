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
	// GB28181 lifecycle counters (library metrics.Hooks seam, SPEC
	// appendix A #38).
	gbRegisterAttempts uint64
	gbRegisterOK       uint64
	gbRegisterFail     uint64
	gbKeepaliveFail    uint64
	gbInviteStarted    uint64
	gbInviteStopped    uint64
	gbInviteFail       uint64
	gbPSBytesOut       uint64
	rtspSessions       uint64
	// Resource gauges (SPEC appendix A #38): pushed by the periodic
	// sampler — the same numbers /api/metrics/summary serves.
	sysCPUPercent   float64
	sysMemTotal     uint64
	sysMemAvailable uint64
	procCPUPercent  float64
	procRSSBytes    uint64
	procOpenFDs     uint64
	netRxBytes      uint64
	netTxBytes      uint64
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

// SetResourceSample pushes one /proc-derived resource sample (SPEC
// appendix A #38) — called from the periodic observe poll.
func (c *Collector) SetResourceSample(sysCPU, procCPU float64, memTotal, memAvail, rss, fds, netRx, netTx uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sysCPUPercent = sysCPU
	c.procCPUPercent = procCPU
	c.sysMemTotal = memTotal
	c.sysMemAvailable = memAvail
	c.procRSSBytes = rss
	c.procOpenFDs = fds
	c.netRxBytes = netRx
	c.netTxBytes = netTx
}

// IncRTSPSession counts one accepted RTSP connection.
func (c *Collector) IncRTSPSession() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rtspSessions++
}

// The GB28181 metrics.Hooks seam (library issue #40). Each maps 1:1 to a
// Prometheus series below; keep them cheap — PSBytesOut fires per media
// packet.
func (c *Collector) RegisterAttempt()      { c.mu.Lock(); c.gbRegisterAttempts++; c.mu.Unlock() }
func (c *Collector) RegisterOK()           { c.mu.Lock(); c.gbRegisterOK++; c.mu.Unlock() }
func (c *Collector) RegisterFail()         { c.mu.Lock(); c.gbRegisterFail++; c.mu.Unlock() }
func (c *Collector) KeepaliveFail()        { c.mu.Lock(); c.gbKeepaliveFail++; c.mu.Unlock() }
func (c *Collector) InviteSessionStarted() { c.mu.Lock(); c.gbInviteStarted++; c.mu.Unlock() }
func (c *Collector) InviteSessionStopped() { c.mu.Lock(); c.gbInviteStopped++; c.mu.Unlock() }
func (c *Collector) InviteFail()           { c.mu.Lock(); c.gbInviteFail++; c.mu.Unlock() }
func (c *Collector) PSBytesOut(n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n > 0 {
		c.gbPSBytesOut += uint64(n)
	}
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
	sysCPUPercent := c.sysCPUPercent
	sysMemTotal := c.sysMemTotal
	sysMemAvail := c.sysMemAvailable
	procCPUPercent := c.procCPUPercent
	procRSSBytes := c.procRSSBytes
	procOpenFDs := c.procOpenFDs
	netRxBytes := c.netRxBytes
	netTxBytes := c.netTxBytes
	gbRegisterAttempts := c.gbRegisterAttempts
	gbRegisterOK := c.gbRegisterOK
	gbRegisterFail := c.gbRegisterFail
	gbKeepaliveFail := c.gbKeepaliveFail
	gbInviteStarted := c.gbInviteStarted
	gbInviteStopped := c.gbInviteStopped
	gbInviteFail := c.gbInviteFail
	gbPSBytesOut := c.gbPSBytesOut
	rtspSessions := c.rtspSessions
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

	// RTSP accepted sessions
	fmt.Fprint(w, "# HELP mibee_eye_rtsp_sessions_total Total accepted RTSP connections\n")
	fmt.Fprint(w, "# TYPE mibee_eye_rtsp_sessions_total counter\n")
	fmt.Fprintf(w, "mibee_eye_rtsp_sessions_total %d\n", rtspSessions)

	// GB28181 lifecycle (library metrics.Hooks seam)
	fmt.Fprint(w, "# HELP mibee_eye_gb28181_register_attempts_total Total REGISTER lifecycles started\n")
	fmt.Fprint(w, "# TYPE mibee_eye_gb28181_register_attempts_total counter\n")
	fmt.Fprintf(w, "mibee_eye_gb28181_register_attempts_total %d\n", gbRegisterAttempts)
	fmt.Fprint(w, "# HELP mibee_eye_gb28181_register_ok_total Total REGISTER lifecycles accepted\n")
	fmt.Fprint(w, "# TYPE mibee_eye_gb28181_register_ok_total counter\n")
	fmt.Fprintf(w, "mibee_eye_gb28181_register_ok_total %d\n", gbRegisterOK)
	fmt.Fprint(w, "# HELP mibee_eye_gb28181_register_fail_total Total REGISTER lifecycles failed\n")
	fmt.Fprint(w, "# TYPE mibee_eye_gb28181_register_fail_total counter\n")
	fmt.Fprintf(w, "mibee_eye_gb28181_register_fail_total %d\n", gbRegisterFail)
	fmt.Fprint(w, "# HELP mibee_eye_gb28181_keepalive_fail_total Total failed keepalive messages\n")
	fmt.Fprint(w, "# TYPE mibee_eye_gb28181_keepalive_fail_total counter\n")
	fmt.Fprintf(w, "mibee_eye_gb28181_keepalive_fail_total %d\n", gbKeepaliveFail)
	fmt.Fprint(w, "# HELP mibee_eye_gb28181_invite_sessions_started_total Total INVITE media sessions confirmed\n")
	fmt.Fprint(w, "# TYPE mibee_eye_gb28181_invite_sessions_started_total counter\n")
	fmt.Fprintf(w, "mibee_eye_gb28181_invite_sessions_started_total %d\n", gbInviteStarted)
	fmt.Fprint(w, "# HELP mibee_eye_gb28181_invite_sessions_stopped_total Total INVITE media sessions stopped\n")
	fmt.Fprint(w, "# TYPE mibee_eye_gb28181_invite_sessions_stopped_total counter\n")
	fmt.Fprintf(w, "mibee_eye_gb28181_invite_sessions_stopped_total %d\n", gbInviteStopped)
	fmt.Fprint(w, "# HELP mibee_eye_gb28181_invite_fail_total Total INVITE sessions that failed to establish\n")
	fmt.Fprint(w, "# TYPE mibee_eye_gb28181_invite_fail_total counter\n")
	fmt.Fprintf(w, "mibee_eye_gb28181_invite_fail_total %d\n", gbInviteFail)
	fmt.Fprint(w, "# HELP mibee_eye_gb28181_ps_bytes_total MPEG-PS bytes handed to the wire\n")
	fmt.Fprint(w, "# TYPE mibee_eye_gb28181_ps_bytes_total counter\n")
	fmt.Fprintf(w, "mibee_eye_gb28181_ps_bytes_total %d\n", gbPSBytesOut)

	// Resource gauges (SPEC appendix A #38) — same sample the JSON
	// summary serves.
	fmt.Fprint(w, "# HELP mibee_eye_system_cpu_percent System CPU busy percent\n")
	fmt.Fprint(w, "# TYPE mibee_eye_system_cpu_percent gauge\n")
	fmt.Fprintf(w, "mibee_eye_system_cpu_percent %g\n", sysCPUPercent)
	fmt.Fprint(w, "# HELP mibee_eye_system_memory_total_bytes System memory total\n")
	fmt.Fprint(w, "# TYPE mibee_eye_system_memory_total_bytes gauge\n")
	fmt.Fprintf(w, "mibee_eye_system_memory_total_bytes %d\n", sysMemTotal)
	fmt.Fprint(w, "# HELP mibee_eye_system_memory_available_bytes System memory available\n")
	fmt.Fprint(w, "# TYPE mibee_eye_system_memory_available_bytes gauge\n")
	fmt.Fprintf(w, "mibee_eye_system_memory_available_bytes %d\n", sysMemAvail)
	fmt.Fprint(w, "# HELP mibee_eye_process_cpu_percent Service process CPU percent\n")
	fmt.Fprint(w, "# TYPE mibee_eye_process_cpu_percent gauge\n")
	fmt.Fprintf(w, "mibee_eye_process_cpu_percent %g\n", procCPUPercent)
	fmt.Fprint(w, "# HELP mibee_eye_process_resident_memory_bytes Service process resident memory\n")
	fmt.Fprint(w, "# TYPE mibee_eye_process_resident_memory_bytes gauge\n")
	fmt.Fprintf(w, "mibee_eye_process_resident_memory_bytes %d\n", procRSSBytes)
	fmt.Fprint(w, "# HELP mibee_eye_process_open_fds Service process open file descriptors\n")
	fmt.Fprint(w, "# TYPE mibee_eye_process_open_fds gauge\n")
	fmt.Fprintf(w, "mibee_eye_process_open_fds %d\n", procOpenFDs)
	fmt.Fprint(w, "# HELP mibee_eye_system_net_rx_bytes Aggregate NIC receive bytes\n")
	fmt.Fprint(w, "# TYPE mibee_eye_system_net_rx_bytes gauge\n")
	fmt.Fprintf(w, "mibee_eye_system_net_rx_bytes %d\n", netRxBytes)
	fmt.Fprint(w, "# HELP mibee_eye_system_net_tx_bytes Aggregate NIC transmit bytes\n")
	fmt.Fprint(w, "# TYPE mibee_eye_system_net_tx_bytes gauge\n")
	fmt.Fprintf(w, "mibee_eye_system_net_tx_bytes %d\n", netTxBytes)
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
