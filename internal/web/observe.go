// Observability backend (SPEC §3.2): real-time system/process resource
// sampling, a bounded log ring, request tracing, and traffic counters.
//
// Real-time only — the sampler keeps the previous sample to compute rates
// and the rings are bounded, so nothing grows with uptime. The /proc
// parsers are pure functions so they unit-test on any machine.
package web

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ── /proc parsers (pure) ─────────────────────────────────────────────

// cpuTimes is the aggregate CPU line of /proc/stat, in ticks.
type cpuTimes struct{ idle, total uint64 }

// parseCPUStat parses the aggregate `cpu` line (idle includes iowait).
func parseCPUStat(content string) (cpuTimes, bool) {
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		var vals []uint64
		for _, f := range fields {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				break
			}
			vals = append(vals, v)
		}
		if len(vals) == 0 {
			return cpuTimes{}, false
		}
		idle := vals[3]
		if len(vals) > 4 {
			idle += vals[4] // iowait
		}
		total := uint64(0)
		for _, v := range vals {
			total += v
		}
		return cpuTimes{idle: idle, total: total}, true
	}
	return cpuTimes{}, false
}

// parseMeminfo returns (MemTotal, MemAvailable) in bytes.
func parseMeminfo(content string) (uint64, uint64, bool) {
	total, avail := uint64(0), uint64(0)
	found := false
	for _, line := range strings.Split(content, "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			fmt.Sscanf(line, "MemTotal: %d kB", &total)
			found = true
		case strings.HasPrefix(line, "MemAvailable:"):
			fmt.Sscanf(line, "MemAvailable: %d kB", &avail)
		}
	}
	if !found {
		return 0, 0, false
	}
	return total * 1024, avail * 1024, true
}

// parseNetDev sums (rx, tx) bytes over physical interfaces (loopback excluded).
func parseNetDev(content string) (uint64, uint64) {
	var rx, tx uint64
	lines := strings.Split(content, "\n")
	if len(lines) > 2 {
		for _, line := range lines[2:] {
			iface, data, found := strings.Cut(line, ":")
			if !found || strings.TrimSpace(iface) == "lo" {
				continue
			}
			fields := strings.Fields(data)
			if len(fields) > 8 {
				r, _ := strconv.ParseUint(fields[0], 10, 64)
				t, _ := strconv.ParseUint(fields[8], 10, 64)
				rx += r
				tx += t
			}
		}
	}
	return rx, tx
}

// parseSelfStat returns (utime, stime) ticks from /proc/<pid>/stat,
// splitting after the last ')' so comms containing spaces parse correctly.
func parseSelfStat(content string) (uint64, uint64, bool) {
	_, rest, found := strings.Cut(content, ")")
	if !found {
		return 0, 0, false
	}
	fields := strings.Fields(rest)
	// After comm: state=field3; utime=field14, stime=field15 → indexes 11,12.
	if len(fields) < 13 {
		return 0, 0, false
	}
	u, err1 := strconv.ParseUint(fields[11], 10, 64)
	st, err2 := strconv.ParseUint(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return u, st, true
}

// parseSelfIO returns (rchar, wchar) from /proc/<pid>/io.
func parseSelfIO(content string) (uint64, uint64, bool) {
	rchar, wchar := uint64(0), uint64(0)
	found := false
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "rchar:") {
			fmt.Sscanf(line, "rchar: %d", &rchar)
			found = true
		} else if strings.HasPrefix(line, "wchar:") {
			fmt.Sscanf(line, "wchar: %d", &wchar)
		}
	}
	if !found {
		return 0, 0, false
	}
	return rchar, wchar, true
}

// cpuPercentBetween renders system CPU busy-percent between two samples.
func cpuPercentBetween(prev, cur cpuTimes) float64 {
	dTotal := cur.total - prev.total
	if prev.total == 0 || dTotal == 0 {
		return 0
	}
	dIdle := cur.idle - prev.idle
	pct := float64(dTotal-dIdle) / float64(dTotal) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

// procCPUPercent renders process CPU percent against numCPUs.
func procCPUPercent(prevTicks, curTicks uint64, dtSecs float64, numCPUs float64) float64 {
	if dtSecs <= 0 || numCPUs <= 0 {
		return 0
	}
	pct := float64(curTicks-prevTicks) / 100.0 / dtSecs * 100 / numCPUs
	if pct < 0 {
		pct = 0
	}
	return pct
}

// ── snapshot & sampling ──────────────────────────────────────────────

// Sample is one raw sampling point (cumulative counters).
type Sample struct {
	ts        time.Time
	cpu       cpuTimes
	procTicks uint64
	net       [2]uint64
}

// Snapshot is the rendered state served by /api/metrics/summary. The
// handler maps these fields into the SPEC JSON shape by hand.
type Snapshot struct {
	TS              int64
	IntervalMS      int64
	SystemCPUPerct  float64
	LoadAvg         []float64
	MemTotal        uint64
	MemAvailable    uint64
	NetRX, NetTX    uint64
	NetRXRate       float64
	NetTXRate       float64
	ProcCPUPerct    float64
	RSSBytes        uint64
	OpenFDs         uint64
	IORead, IOWrite uint64
}

// readSample collects one raw sample from the live system.
func readSample() Sample {
	s := Sample{ts: time.Now()}
	if data, err := os.ReadFile("/proc/stat"); err == nil {
		if ct, ok := parseCPUStat(string(data)); ok {
			s.cpu = ct
		}
	}
	if data, err := os.ReadFile("/proc/self/stat"); err == nil {
		if u, st, ok := parseSelfStat(string(data)); ok {
			s.procTicks = u + st
		}
	}
	if data, err := os.ReadFile("/proc/net/dev"); err == nil {
		rx, tx := parseNetDev(string(data))
		s.net = [2]uint64{rx, tx}
	}
	return s
}

// ── rings & shared state ─────────────────────────────────────────────

// LogEntry is one captured log line (SPEC §3.2 /api/logs).
type LogEntry struct {
	TS    int64  `json:"ts"`
	Level string `json:"level"`
	Raw   string `json:"message"`
}

// RequestEntry is one traced Web API call (SPEC §3.2 /api/requests).
type RequestEntry struct {
	ID         string  `json:"id"`
	Method     string  `json:"method"`
	Path       string  `json:"path"`
	Status     int     `json:"status"`
	DurationMS float64 `json:"duration_ms"`
	TS         int64   `json:"ts"`
}

// Observe is the shared observability state owned by the web Server.
type Observe struct {
	mu       sync.Mutex
	snapshot Snapshot
	prev     *Sample
	logs     []LogEntry
	requests []RequestEntry

	HTTPRX, HTTPTX atomic.Uint64
	RTSPTX         atomic.Uint64
	GB28181TX      atomic.Uint64

	nextReqID atomic.Uint64
	logCap    int
	reqCap    int
}

// NewObserve creates the shared state with SPEC ring capacities.
func NewObserve() *Observe {
	return &Observe{logCap: 1000, reqCap: 500}
}

// SampleNow records one sample and re-renders the snapshot (used by the
// background sampler and by tests).
func (o *Observe) SampleNow() {
	cur := readSample()
	o.mu.Lock()
	defer o.mu.Unlock()
	prev := o.prev
	o.prev = &cur
	snap := Snapshot{TS: cur.ts.Unix()}
	if prev != nil && dtSeconds(prev, &cur) > 0 {
		dt := dtSeconds(prev, &cur)
		snap.IntervalMS = int64(dt * 1000)
		snap.SystemCPUPerct = cpuPercentBetween(prev.cpu, cur.cpu)
		snap.ProcCPUPerct = procCPUPercent(prev.procTicks, cur.procTicks, dt, float64(runtime.NumCPU()))
		snap.NetRXRate = float64(cur.net[0]-prev.net[0]) / dt
		snap.NetTXRate = float64(cur.net[1]-prev.net[1]) / dt
	}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		if total, avail, ok := parseMeminfo(string(data)); ok {
			snap.MemTotal, snap.MemAvailable = total, avail
		}
	}
	snap.LoadAvg = readLoadAvg()
	if data, err := os.ReadFile("/proc/self/io"); err == nil {
		snap.IORead, snap.IOWrite, _ = parseSelfIO(string(data))
	}
	snap.RSSBytes = readRSSBytes()
	snap.OpenFDs = countOpenFDs()
	snap.NetRX, snap.NetTX = cur.net[0], cur.net[1]
	o.snapshot = snap
}

// dtSeconds returns the elapsed seconds between two samples.
func dtSeconds(prev, cur *Sample) float64 {
	return cur.ts.Sub(prev.ts).Seconds()
}

// Snapshot returns the latest rendered snapshot.
func (o *Observe) Snapshot() Snapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.snapshot
}

// AddLog captures one formatted log line into the ring.
func (o *Observe) AddLog(level, raw string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.logs) == o.logCap {
		o.logs = o.logs[1:]
	}
	o.logs = append(o.logs, LogEntry{
		TS:    time.Now().Unix(),
		Level: level,
		Raw:   raw,
	})
}

// LogsNewestFirst returns a copy of the log ring, newest first.
func (o *Observe) LogsNewestFirst() []LogEntry {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]LogEntry, 0, len(o.logs))
	for i := len(o.logs) - 1; i >= 0; i-- {
		out = append(out, o.logs[i])
	}
	return out
}

// AddRequest records one traced API call.
func (o *Observe) AddRequest(e RequestEntry) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.requests) == o.reqCap {
		o.requests = o.requests[1:]
	}
	o.requests = append(o.requests, e)
}

// RequestsNewestFirst returns a copy of the trace ring, newest first.
func (o *Observe) RequestsNewestFirst() []RequestEntry {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]RequestEntry, 0, len(o.requests))
	for i := len(o.requests) - 1; i >= 0; i-- {
		out = append(out, o.requests[i])
	}
	return out
}

// AllocRequestID returns the next short hex request id.
func (o *Observe) AllocRequestID() string {
	return fmt.Sprintf("%06x", o.nextReqID.Add(1)-1)
}

// RunSampler refreshes the snapshot every interval until ctx is done.
func (o *Observe) RunSampler(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	o.SampleNow()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.SampleNow()
		}
	}
}

func readLoadAvg() []float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return []float64{0, 0, 0}
	}
	fields := strings.Fields(string(data))
	out := make([]float64, 0, 3)
	for i := 0; i < 3 && i < len(fields); i++ {
		v, _ := strconv.ParseFloat(fields[i], 64)
		out = append(out, v)
	}
	for len(out) < 3 {
		out = append(out, 0)
	}
	return out
}

func readRSSBytes() uint64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			var kb uint64
			fmt.Sscanf(line, "VmRSS: %d kB", &kb)
			return kb * 1024
		}
	}
	return 0
}

func countOpenFDs() uint64 {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return uint64(len(entries))
}

// diskUsage stats a mount via statfs(2): (total, used, free).
func diskUsage(path string) (uint64, uint64, uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, 0, false
	}
	bsize := uint64(st.Bsize)
	total := st.Blocks * bsize
	free := st.Bfree * bsize
	used := total - st.Bavail*bsize
	return total, used, free, true
}

// ── log capture ──────────────────────────────────────────────────────

// levelFromLine extracts a coarse level from a formatted log line (slog
// default text puts `level=INFO`; stdlib log prefixes `[INFO]`-style words
// in this project's formats). Everything unknown counts as info.
func levelFromLine(line string) string {
	for _, pair := range [][2]string{
		{"level=DEBUG", "debug"}, {"level=INFO", "info"},
		{"level=WARN", "warn"}, {"level=ERROR", "error"},
		{"[DEBUG]", "debug"}, {"[INFO]", "info"}, {"[WARNING]", "warn"},
		{"[ERROR]", "error"}, {"DEBUG:", "debug"}, {"WARNING:", "warn"},
		{"ERROR:", "error"},
	} {
		if strings.Contains(line, pair[0]) {
			return pair[1]
		}
	}
	return "info"
}

// AttachLogRing directs the process log output through a writer that tees
// every line into the observability ring (and keeps stdout). Call once at
// startup, before any logging matters. The default slog handler routes
// through the stdlib log package, so this captures both.
func (o *Observe) AttachLogRing() {
	log.SetOutput(&teeWriter{next: os.Stdout, observe: o})
}

// SlogTeeHandler wraps another slog.Handler: every record is appended to
// the observability ring (formatted `message key=value …`) and then
// delegated unchanged. Use it to wrap the production slog handler so
// /api/logs mirrors journald output (SPEC §3.2).
type SlogTeeHandler struct {
	inner   slog.Handler
	observe *Observe
	attrs   string
	group   string
}

// NewSlogTeeHandler wraps inner so records tee into the ring.
func NewSlogTeeHandler(inner slog.Handler, observe *Observe) *SlogTeeHandler {
	return &SlogTeeHandler{inner: inner, observe: observe}
}

// Enabled reports whether the wrapped handler would emit at this level.
func (h *SlogTeeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle tees one record into the ring, then delegates.
func (h *SlogTeeHandler) Handle(ctx context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	h.observe.AddLog(strings.ToLower(r.Level.String()), b.String())
	return h.inner.Handle(ctx, r)
}

// WithAttrs returns a handler whose subsequent records carry the attrs.
func (h *SlogTeeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.attrs)
	for _, a := range attrs {
		b.WriteString(" " + a.Key + "=" + a.Value.String())
	}
	return &SlogTeeHandler{inner: h.inner.WithAttrs(attrs), observe: h.observe, attrs: b.String(), group: h.group}
}

// WithGroup returns a handler scoped to a named group.
func (h *SlogTeeHandler) WithGroup(name string) slog.Handler {
	return &SlogTeeHandler{inner: h.inner.WithGroup(name), observe: h.observe, attrs: h.attrs, group: name}
}

type teeWriter struct {
	next    io.Writer
	observe *Observe
}

func (t *teeWriter) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	t.observe.AddLog(levelFromLine(line), line)
	return t.next.Write(p)
}

// ── HTTP middleware & handlers ───────────────────────────────────────

// statusInterceptor captures the response code and byte count while
// forwarding the Flush/Hijack capabilities the streaming endpoints need.
type statusInterceptor struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusInterceptor) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusInterceptor) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

func (w *statusInterceptor) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// observeMiddleware wraps the mux with request tracing (SPEC §3.2): a
// request id echoed via X-Request-Id, a trace entry per /api call, and
// app-attributed traffic counters.
func (s *Server) observeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := s.observe.AllocRequestID()
		w.Header().Set("X-Request-Id", id)
		intercepted := &statusInterceptor{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(intercepted, r)
		if r.ContentLength > 0 {
			s.observe.HTTPRX.Add(uint64(r.ContentLength))
		}
		if intercepted.bytes > 0 {
			s.observe.HTTPTX.Add(uint64(intercepted.bytes))
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			status := intercepted.status
			if status == 0 {
				status = http.StatusOK
			}
			s.observe.AddRequest(RequestEntry{
				ID:         id,
				Method:     r.Method,
				Path:       r.URL.Path,
				Status:     status,
				DurationMS: float64(time.Since(start).Microseconds()) / 1000.0,
				TS:         time.Now().Unix(),
			})
		}
	})
}

// handleMetricsSummary (GET /api/metrics/summary): SPEC §3.2 snapshot.
func (s *Server) handleMetricsSummary(w http.ResponseWriter, r *http.Request) {
	snap := s.observe.Snapshot()
	traffic := map[string]uint64{
		"http_rx_bytes":    s.observe.HTTPRX.Load(),
		"http_tx_bytes":    s.observe.HTTPTX.Load(),
		"rtsp_tx_bytes":    s.observe.RTSPTX.Load(),
		"gb28181_tx_bytes": s.observe.GB28181TX.Load(),
	}
	disks := []interface{}{}
	if total, used, free, ok := diskUsage("/"); ok {
		disks = append(disks, map[string]interface{}{
			"path": "/", "total": total, "used": used, "free": free,
		})
	}
	writeOK(w, http.StatusOK, map[string]interface{}{
		"ts":          snap.TS,
		"interval_ms": snap.IntervalMS,
		"system": map[string]interface{}{
			"cpu_percent": snap.SystemCPUPerct,
			"load_avg":    snap.LoadAvg,
			"memory": map[string]uint64{
				"total":     snap.MemTotal,
				"used":      snap.MemTotal - snap.MemAvailable,
				"available": snap.MemAvailable,
			},
			"disks": disks,
			"network": map[string]interface{}{
				"rx_bytes": snap.NetRX, "tx_bytes": snap.NetTX,
				"rx_rate": snap.NetRXRate, "tx_rate": snap.NetTXRate,
			},
		},
		"process": map[string]interface{}{
			"cpu_percent":    snap.ProcCPUPerct,
			"rss_bytes":      snap.RSSBytes,
			"open_fds":       snap.OpenFDs,
			"uptime":         int64(time.Since(s.startTime).Seconds()),
			"io_read_bytes":  snap.IORead,
			"io_write_bytes": snap.IOWrite,
			"storage_bytes":  uint64(0),
			"traffic":        traffic,
		},
	})
}

// handleLogs (GET /api/logs): SPEC §3.2 ring dump with limit/level filters.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit := clampLimit(r.URL.Query().Get("limit"), 200, 1000)
	minRank := levelRank(r.URL.Query().Get("level"))
	entries := s.observe.LogsNewestFirst()
	out := make([]LogEntry, 0, limit)
	for _, e := range entries {
		if len(out) == limit {
			break
		}
		if levelRank(e.Level) < minRank {
			continue
		}
		out = append(out, e)
	}
	writeOK(w, http.StatusOK, map[string]interface{}{"entries": out})
}

// handleRequests (GET /api/requests): SPEC §3.2 trace ring dump.
func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	limit := clampLimit(r.URL.Query().Get("limit"), 100, 500)
	entries := s.observe.RequestsNewestFirst()
	if len(entries) > limit {
		entries = entries[:limit]
	}
	writeOK(w, http.StatusOK, map[string]interface{}{"entries": entries})
}

func clampLimit(raw string, def, max int) int {
	n := def
	if raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			n = v
		}
	}
	if n > max {
		n = max
	}
	return n
}

func levelRank(level string) int {
	ranks := map[string]int{"debug": 0, "info": 1, "warn": 2, "error": 3}
	r, ok := ranks[strings.ToLower(level)]
	if !ok {
		return 0
	}
	return r
}
