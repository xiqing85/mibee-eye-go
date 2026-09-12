package web

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// ── parsers ──────────────────────────────────────────────────────────

const cpuStatFixture = "cpu  100 0 100 300 100 0 0 0 0 0\ncpu0 50 0 50 150 50 0 0 0 0 0\nintr 123\n"

func TestParseCPUStatIncludesIowaitInIdle(t *testing.T) {
	ct, ok := parseCPUStat(cpuStatFixture)
	if !ok {
		t.Fatal("must parse")
	}
	// idle=300 iowait=100 → 400; total = 100+0+100+300+100 = 600.
	if ct.idle != 400 || ct.total != 600 {
		t.Fatalf("idle=%d total=%d, want 400/600", ct.idle, ct.total)
	}
}

func TestCPUPercentBetween(t *testing.T) {
	prev := cpuTimes{idle: 400, total: 600}
	cur := cpuTimes{idle: 550, total: 800} // +200 total, +150 idle → 25%
	if got := cpuPercentBetween(prev, cur); got < 24.99 || got > 25.01 {
		t.Fatalf("got %v, want 25", got)
	}
	if got := cpuPercentBetween(prev, prev); got != 0 {
		t.Fatalf("no-change must be 0, got %v", got)
	}
}

func TestParseMeminfo(t *testing.T) {
	total, avail, ok := parseMeminfo("MemTotal:       8000000 kB\nMemFree:        100000 kB\nMemAvailable:   3000000 kB\n")
	if !ok || total != 8_000_000*1024 || avail != 3_000_000*1024 {
		t.Fatalf("total=%d avail=%d ok=%v", total, avail, ok)
	}
}

func TestParseNetDevSkipsLoopback(t *testing.T) {
	// 8 receive stats then 8 transmit stats per line.
	src := "Inter-|   Receive  |  Transmit\n" +
		" face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier\n" +
		"  lo:  999999 999 0 0 0 0 0 0 9999 999 0 0 0 0 0 0\n" +
		" eth0: 150000 200 0 0 0 0 0 0 90000 150 0 0 0 0 0 0\n"
	rx, tx := parseNetDev(src)
	if rx != 150_000 || tx != 90_000 {
		t.Fatalf("rx=%d tx=%d, want 150000/90000", rx, tx)
	}
}

func TestParseSelfStatWithSpacesInComm(t *testing.T) {
	u, st, ok := parseSelfStat("42 (camera worke) S 1 2 3 0 -1 4194560 100 0 0 0 77 33 0 0 20 0 4 0 123456 1 1")
	if !ok || u != 77 || st != 33 {
		t.Fatalf("utime=%d stime=%d ok=%v", u, st, ok)
	}
}

func TestParseSelfIO(t *testing.T) {
	r, w, ok := parseSelfIO("rchar: 123456\nwchar: 654321\nsyscr: 100\n")
	if !ok || r != 123_456 || w != 654_321 {
		t.Fatalf("rchar=%d wchar=%d ok=%v", r, w, ok)
	}
}

func TestProcCPUPercentScalesByCores(t *testing.T) {
	if got := procCPUPercent(0, 100, 1.0, 1.0); got != 100 {
		t.Fatalf("1 core: %v", got)
	}
	if got := procCPUPercent(0, 100, 1.0, 4.0); got != 25 {
		t.Fatalf("4 cores: %v", got)
	}
}

// ── rings & sampling ─────────────────────────────────────────────────

func TestSampleNowRendersSnapshot(t *testing.T) {
	o := NewObserve()
	o.SampleNow()
	time.Sleep(20 * time.Millisecond)
	o.SampleNow()
	snap := o.Snapshot()
	if snap.TS == 0 {
		t.Fatal("snapshot ts must be set")
	}
	if snap.MemTotal == 0 {
		t.Fatal("meminfo must parse on linux")
	}
	if snap.RSSBytes == 0 {
		t.Fatal("VmRSS must parse")
	}
	if snap.OpenFDs == 0 {
		t.Fatal("open fds must be counted")
	}
	if snap.IntervalMS <= 0 {
		t.Fatalf("interval must be measured, got %d", snap.IntervalMS)
	}
}

func TestLogRingCapacity(t *testing.T) {
	o := NewObserve()
	o.logCap = 3
	for i := 0; i < 5; i++ {
		o.AddLog("info", "line")
	}
	if got := len(o.LogsNewestFirst()); got != 3 {
		t.Fatalf("cap enforced, got %d", got)
	}
}

func TestLevelFromLine(t *testing.T) {
	cases := map[string]string{
		`time=2026-09-02 level=WARN msg="x"`: "warn",
		"2026/09/02 12:00:00 [INFO] web: hi": "info",
		"anything else":                      "info",
		"level=ERROR msg=boom":               "error",
	}
	for line, want := range cases {
		if got := levelFromLine(line); got != want {
			t.Fatalf("%q: got %s want %s", line, got, want)
		}
	}
}

func TestAttachLogRingCapturesBothLoggers(t *testing.T) {
	o := NewObserve()
	prev := log.Writer()
	log.SetFlags(0)
	o.AttachLogRing()
	t.Cleanup(func() {
		log.SetOutput(prev)
		log.SetFlags(log.LstdFlags)
	})
	log.Printf("[INFO] stdlib line")
	log.Printf(`level=INFO msg="slog line"`)
	logs := o.LogsNewestFirst()
	found := 0
	for _, e := range logs {
		if strings.Contains(e.Raw, "stdlib line") || strings.Contains(e.Raw, "slog line") {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("expected both lines in ring, got %d of %v", found, logs)
	}
	if _, err := os.Stdout.Stat(); err != nil {
		t.Fatalf("stdout writer must stay alive: %v", err)
	}
}

// ── HTTP surface ─────────────────────────────────────────────────────

func TestObservabilityEndpoints(t *testing.T) {
	srv := New(Config{Username: "admin", Password: "spec-pass-1"})
	srv.observe.SampleNow()

	// Full chain (tracing middleware + routes) over real HTTP.
	mux := http.NewServeMux()
	srv.mux = mux
	srv.registerRoutes()
	ts := httptest.NewServer(srv.observeMiddleware(mux))
	t.Cleanup(ts.Close)

	// Unauthenticated reads must be rejected.
	res, err := http.Get(ts.URL + "/api/metrics/summary")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("summary must be session-gated, got %d", res.StatusCode)
	}

	// Log in through the real endpoint.
	res, err = http.Post(ts.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"spec-pass-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	cookies := res.Cookies()
	res.Body.Close()
	session := ""
	for _, c := range cookies {
		if c.Name == "session" {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("login must issue session cookie")
	}
	authedGet := func(path string) (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.AddCookie(&http.Cookie{Name: "session", Value: session})
		return http.DefaultClient.Do(req)
	}

	// summary shape (SPEC §3.2)
	res, err = authedGet("/api/metrics/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("summary: %d", res.StatusCode)
	}
	var body struct {
		OK   bool `json:"ok"`
		Data struct {
			TS         int64 `json:"ts"`
			IntervalMS int64 `json:"interval_ms"`
			System     struct {
				CPUPercent float64 `json:"cpu_percent"`
				Memory     struct {
					Total uint64 `json:"total"`
				} `json:"memory"`
				Network struct {
					RXRate float64 `json:"rx_rate"`
				} `json:"network"`
				Disks []struct {
					Path string `json:"path"`
				} `json:"disks"`
			} `json:"system"`
			Process struct {
				CPUPercent   float64 `json:"cpu_percent"`
				RSSBytes     uint64  `json:"rss_bytes"`
				OpenFDs      uint64  `json:"open_fds"`
				Uptime       int64   `json:"uptime"`
				StorageBytes uint64  `json:"storage_bytes"`
				IOReadBytes  uint64  `json:"io_read_bytes"`
				Traffic      struct {
					HTTPTX uint64 `json:"http_tx_bytes"`
				} `json:"traffic"`
			} `json:"process"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || body.Data.TS == 0 || body.Data.System.Memory.Total == 0 || len(body.Data.System.Disks) == 0 {
		t.Fatalf("summary shape broken: %+v", body.Data)
	}
	if body.Data.Process.RSSBytes == 0 {
		t.Fatal("rss must be positive")
	}

	// requests ring carries the authed calls above (login is /api too)
	res2, err := authedGet("/api/requests?limit=10")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	var reqBody struct {
		Data struct {
			Entries []RequestEntry `json:"entries"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res2.Body).Decode(&reqBody); err != nil {
		t.Fatal(err)
	}
	if len(reqBody.Data.Entries) == 0 {
		t.Fatal("trace ring must have entries")
	}
	// The handler's own entry is recorded after it responds, so the newest
	// entry at read time is the previous call (the summary).
	if reqBody.Data.Entries[0].Path != "/api/metrics/summary" {
		t.Fatalf("newest first: %+v", reqBody.Data.Entries[0])
	}

	// logs endpoint answers with level filter (ring may be empty)
	res3, err := authedGet("/api/logs?limit=5&level=warn")
	if err != nil {
		t.Fatal(err)
	}
	defer res3.Body.Close()
	if res3.StatusCode != http.StatusOK {
		t.Fatalf("logs: %d", res3.StatusCode)
	}
}

func TestObserveMiddlewareTracesAndCounts(t *testing.T) {
	srv := New(Config{})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
	})
	ts := httptest.NewServer(srv.observeMiddleware(mux))
	t.Cleanup(ts.Close)

	res, err := http.Post(ts.URL+"/api/ping", "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get("X-Request-Id") == "" {
		t.Fatal("X-Request-Id must be echoed")
	}
	if srv.observe.HTTPRX.Load() == 0 {
		t.Fatal("rx bytes must be counted from content-length")
	}
	if srv.observe.HTTPTX.Load() != 4 {
		t.Fatalf("tx bytes: %d", srv.observe.HTTPTX.Load())
	}
	entries := srv.observe.RequestsNewestFirst()
	if len(entries) != 1 || entries[0].Path != "/api/ping" || entries[0].Status != 200 {
		t.Fatalf("trace entry: %+v", entries)
	}
}

// The sampler loop must stop when the context is cancelled.
func TestRunSamplerStopsOnContextCancel(t *testing.T) {
	o := NewObserve()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.RunSampler(ctx, time.Hour)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sampler must exit on context cancel")
	}
}
