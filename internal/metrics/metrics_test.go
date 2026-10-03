package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCollectorRendersGBAndResourceFamilies(t *testing.T) {
	c := NewCollector()
	c.RegisterAttempt()
	c.RegisterOK()
	c.RegisterAttempt()
	c.RegisterFail()
	c.KeepaliveFail()
	c.InviteSessionStarted()
	c.InviteSessionStopped()
	c.InviteFail()
	c.PSBytesOut(1500)
	c.IncRTSPSession()
	c.SetResourceSample(23.5, 12.25, 8_000_000, 3_000_000, 999_000, 42, 1_000, 2_000)

	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()

	for _, want := range []string{
		"mibee_eye_gb28181_register_attempts_total 2",
		"mibee_eye_gb28181_register_ok_total 1",
		"mibee_eye_gb28181_register_fail_total 1",
		"mibee_eye_gb28181_keepalive_fail_total 1",
		"mibee_eye_gb28181_invite_sessions_started_total 1",
		"mibee_eye_gb28181_invite_sessions_stopped_total 1",
		"mibee_eye_gb28181_invite_fail_total 1",
		"mibee_eye_gb28181_ps_bytes_total 1500",
		"mibee_eye_rtsp_sessions_total 1",
		"mibee_eye_system_cpu_percent 23.5",
		"mibee_eye_system_memory_total_bytes 8000000",
		"mibee_eye_system_memory_available_bytes 3000000",
		"mibee_eye_process_cpu_percent 12.25",
		"mibee_eye_process_resident_memory_bytes 999000",
		"mibee_eye_process_open_fds 42",
		"mibee_eye_system_net_rx_bytes 1000",
		"mibee_eye_system_net_tx_bytes 2000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

// Compile-time proof that Collector satisfies the library's metrics.Hooks
// seam (device.SetMetricsHooks) — the hot-path contract keeps methods cheap.
var _ interface {
	RegisterAttempt()
	RegisterOK()
	RegisterFail()
	KeepaliveFail()
	InviteSessionStarted()
	InviteSessionStopped()
	InviteFail()
	PSBytesOut(n int64)
} = (*Collector)(nil)
