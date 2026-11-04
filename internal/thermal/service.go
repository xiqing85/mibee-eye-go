package thermal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/xiqing85/mibee-eye-go/internal/i2c"
	"gopkg.in/yaml.v3"
)

// BusSpec is the thermal bus wiring (SPEC §3.7 hardware dialect):
// type=twi (device), type=gpio (sda/scl sysfs bit-bang) or type=auto.
type BusSpec struct {
	Type   string `yaml:"type" json:"type"`
	Device string `yaml:"device,omitempty" json:"device,omitempty"`
	Sda    uint32 `yaml:"sda,omitempty" json:"sda,omitempty"`
	Scl    uint32 `yaml:"scl,omitempty" json:"scl,omitempty"`
}

// BusYAML renders the inline YAML mapping form (used by scan snippets).
func BusYAML(b *BusSpec) string {
	switch b.Type {
	case "twi":
		return fmt.Sprintf("{type: twi, device: %q}", b.Device)
	case "gpio":
		return fmt.Sprintf("{type: gpio, sda: %d, scl: %d}", b.Sda, b.Scl)
	default:
		return "{type: auto}"
	}
}

// Config is the [thermal] product section.
type Config struct {
	Enabled    bool    `yaml:"enabled"`
	Bus        BusSpec `yaml:"bus"`
	Addr       uint8   `yaml:"addr"`
	Emissivity float64 `yaml:"emissivity"`
}

// SensorTarget is the runtime bus+addr, hot-reconfigured by the hardware
// API and re-read by the poll loop every pass.
type SensorTarget struct {
	Bus  BusSpec `json:"bus"`
	Addr uint8   `json:"addr"`
}

// ScanState is one scan run's outcome.
type ScanState struct {
	Running    bool     `json:"running"`
	StartedMs  int64    `json:"started_ms"`
	FinishedMs *int64   `json:"finished_ms"`
	Hits       []Hit    `json:"hits"`
	PairErrors []string `json:"pair_errors"`
	Error      *string  `json:"error"`
}

// goodFrame backs the untrusted payload shape (SPEC §3.7 M4 dialect:
// trusted:false + last_known_good + reason).
type goodFrame struct {
	TimestampMs int64   `json:"timestamp_ms"`
	Ta          float64 `json:"ta"`
	To          [Pixels]float32
}

type readingState struct {
	FrameNumber uint64
	TimestampMs int64
	Trusted     bool
	Reason      string
	Ta, Vdd     float64
	To          [Pixels]float32
	SensorFPS   float32
	LastGood    *goodFrame
}

// Service is the thermal subsystem: poll loop + shared state + the
// hardware bring-up control block.
type Service struct {
	mu     sync.RWMutex
	state  readingState
	hwMu   sync.Mutex // bus ownership: poll pass vs scan sweep
	target sync.RWMutex
	tgt    SensorTarget
	scanMu sync.Mutex
	scan   ScanState

	persistPath string
	stop        chan struct{}
	ee          *Eeprom
	haveEE      bool
	failures    uint32
	lastTarget  SensorTarget
	emissivity  float64
}

func nowMs() int64 { return time.Now().UnixMilli() }

// NewService builds the service (inert until Run).
func NewService(cfg *Config) *Service {
	return &Service{
		state: readingState{
			TimestampMs: nowMs(),
			Trusted:     false,
			Reason:      "warmup",
		},
		tgt:        SensorTarget{Bus: cfg.Bus, Addr: cfg.Addr},
		stop:       make(chan struct{}),
		lastTarget: SensorTarget{Bus: cfg.Bus, Addr: cfg.Addr},
		emissivity: cfg.Emissivity,
	}
}

// SetPersistPath wires the overlay file (boot wiring, before serving).
func (s *Service) SetPersistPath(path string) { s.persistPath = path }

// Run polls the sensor until ctx ends: one pass per iteration (bus opened
// and dropped per pass), failures back off but keep last_known_good.
func (s *Service) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		default:
		}
		// Bus ownership: a scan sweep holds hwMu — idle until it's done.
		if !s.hwMu.TryLock() {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		s.passOnce(ctx)
		s.hwMu.Unlock()
		backoff := time.Second
		if s.failures > 0 {
			bo := s.failures
			if bo > 30 {
				bo = 30
			}
			backoff = time.Duration(bo) * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-time.After(backoff):
		}
	}
}

// Stop ends the poll loop (shutdown step).
func (s *Service) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
}

func (s *Service) passOnce(ctx context.Context) {
	started := time.Now()
	s.target.RLock()
	target := s.tgt
	s.target.RUnlock()
	if target != s.lastTarget {
		s.haveEE = false // swapped sensor → stale calibration
		s.lastTarget = target
		slog.Info("thermal: target changed, re-reading EEPROM", "bus", target.Bus.Type)
	}
	to, common, ok := s.readPass(target)
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.failures = 0
		el := time.Since(started).Seconds()
		if el < 1.0/32.0 {
			el = 1.0 / 32.0
		}
		s.state.SensorFPS = float32(1.0 / el)
		s.state.FrameNumber++
		s.state.TimestampMs = nowMs()
		s.state.Trusted = true
		s.state.Ta = common.Ta
		s.state.Vdd = common.Vdd
		s.state.To = to
		s.state.LastGood = &goodFrame{TimestampMs: s.state.TimestampMs, Ta: common.Ta, To: to}
		if s.state.FrameNumber == 1 {
			slog.Info("thermal: first real frame landed", "ta", common.Ta)
		}
		return
	}
	s.failures++
	s.state.SensorFPS = 0
	s.state.TimestampMs = nowMs()
	s.state.Trusted = false
	s.state.Reason = "sensor_lost"
	if s.failures == 1 {
		slog.Warn("thermal: sensor pass failed (bus or wiring)")
	}
}

func (s *Service) readPass(target SensorTarget) ([Pixels]float32, CommonParams, bool) {
	bus, err := openBus(target.Bus, target.Addr)
	if err != nil {
		return [Pixels]float32{}, CommonParams{}, false
	}
	if c, ok := bus.(ioCloser); ok {
		defer c.Close()
	}
	sensor := NewMlx90640(bus)
	if !s.haveEE {
		words, err := sensor.ReadEEPROM()
		if err != nil {
			return [Pixels]float32{}, CommonParams{}, false
		}
		s.ee = EepromFromWords(words)
		s.haveEE = true
	}
	aux, err := sensor.ReadAux()
	if err != nil {
		return [Pixels]float32{}, CommonParams{}, false
	}
	frame, err := sensor.ReadFrameRaw()
	if err != nil {
		return [Pixels]float32{}, CommonParams{}, false
	}
	to, common := s.ee.ComputeTo(&frame, &aux, s.emissivity, 0)
	return to, common, true
}

type ioCloser interface{ Close() error }

func openBus(spec BusSpec, addr uint8) (i2c.Bus, error) {
	switch spec.Type {
	case "twi":
		return i2c.OpenTwi(spec.Device, addr)
	case "gpio":
		ReleaseGpioPair(spec.Sda, spec.Scl)
		return i2c.NewBitBang(spec.Sda, spec.Scl, addr)
	default: // auto
		hit, _ := AutoDetect()
		if hit == nil {
			return nil, fmt.Errorf("thermal: auto-detect found no MLX90640 (run a scan from the hardware card)")
		}
		bus := hitBus(hit)
		return openBus(bus, hit.Addr)
	}
}

// Available reports whether a trusted frame has landed.
func (s *Service) Available() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Trusted
}

// Radiometric reports whether any real frame was ever decoded.
func (s *Service) Radiometric() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.LastGood != nil
}

// Grid returns the latest To grid (rendering input).
func (s *Service) Grid() [Pixels]float32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.To
}

// Payload builds the SPEC §3.7 document (current with grid=true; the SSE
// shape without).
func (s *Service) Payload(grid bool) map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.state
	maxI, minI := argExtremes(&st.To)
	doc := map[string]any{
		"timestamp_ms": st.TimestampMs,
		"frame_number": st.FrameNumber,
		"trusted":      st.Trusted,
		"ta":           round(st.Ta, 2),
		"vdd":          round(st.Vdd, 3),
		"sensor_fps":   round(float64(st.SensorFPS), 3),
		"max":          spotJSON(&st.To, maxI),
		"min":          spotJSON(&st.To, minI),
		"spots":        []any{},
	}
	if !st.Trusted {
		doc["reason"] = st.Reason
		if st.LastGood != nil {
			gMax, gMin := argExtremes(&st.LastGood.To)
			doc["last_known_good"] = map[string]any{
				"timestamp_ms": st.LastGood.TimestampMs,
				"ta":           round(st.LastGood.Ta, 2),
				"max":          spotJSON(&st.LastGood.To, gMax),
				"min":          spotJSON(&st.LastGood.To, gMin),
				"age_ms":       st.TimestampMs - st.LastGood.TimestampMs,
			}
		}
	}
	if grid {
		doc["grid"] = map[string]any{
			"w": Width, "h": Height, "unit": "deg_c",
			"values": st.To[:],
		}
	}
	return doc
}

func round(v float64, places int) float64 {
	mult := 1.0
	for i := 0; i < places; i++ {
		mult *= 10
	}
	return float64(int64(v*mult+0.5*sign(v))) / mult
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

func spotJSON(to *[Pixels]float32, i int) map[string]any {
	return map[string]any{
		"value_c": round(float64(to[i]), 1),
		"x":       i % Width,
		"y":       i / Width,
	}
}

func argExtremes(to *[Pixels]float32) (int, int) {
	maxI, minI := 0, 0
	for i, v := range to {
		if v > to[maxI] {
			maxI = i
		}
		if v < to[minI] {
			minI = i
		}
	}
	return maxI, minI
}

// ————— hardware bring-up API —————

// Target returns the live sensor target.
func (s *Service) Target() SensorTarget {
	s.target.RLock()
	defer s.target.RUnlock()
	return s.tgt
}

// SetTarget applies a new target at runtime (next pass uses it).
func (s *Service) SetTarget(t SensorTarget) {
	s.target.Lock()
	s.tgt = t
	s.target.Unlock()
}

// ScanState returns the last/current sweep.
func (s *Service) ScanState() ScanState {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	return s.scan.clone()
}

func (st *ScanState) clone() ScanState {
	c := *st
	c.Hits = append([]Hit(nil), st.Hits...)
	c.PairErrors = append([]string(nil), st.PairErrors...)
	return c
}

// ScanJSON renders the ScanState for the API (hits carry paste-ready
// snippets).
func ScanJSON(st ScanState) map[string]any {
	hits := make([]map[string]any, 0, len(st.Hits))
	for i := range st.Hits {
		h := st.Hits[i]
		hits = append(hits, map[string]any{
			"bus": h.Bus, "addr": h.Addr, "id": h.ID,
			"config": ConfigSnippet(&h),
		})
	}
	return map[string]any{
		"running":     st.Running,
		"started_ms":  st.StartedMs,
		"finished_ms": st.FinishedMs,
		"hits":        hits,
		"pair_errors": st.PairErrors,
		"error":       st.Error,
	}
}

// StartScan kicks a background sweep (false if one already runs). The
// poll loop yields the bus (single-mutex ownership) for the duration.
func (s *Service) StartScan(gpioCandidates [][2]uint32) bool {
	s.scanMu.Lock()
	if s.scan.Running {
		s.scanMu.Unlock()
		return false
	}
	s.scan = ScanState{Running: true, StartedMs: nowMs(), Hits: []Hit{}, PairErrors: []string{}}
	s.scanMu.Unlock()

	go func() {
		hits, pairErrs, sweepErr := s.scanSweep(gpioCandidates)
		s.scanMu.Lock()
		done := nowMs()
		s.scan = ScanState{
			StartedMs: s.scan.StartedMs, FinishedMs: &done,
			Hits: hits, PairErrors: pairErrs, Error: sweepErr,
		}
		s.scanMu.Unlock()
	}()
	return true
}

func (s *Service) scanSweep(cands [][2]uint32) ([]Hit, []string, *string) {
	// Acquire bus ownership; a bit-bang frame pass holds it for ~40 s.
	deadline := time.Now().Add(90 * time.Second)
	for !s.hwMu.TryLock() {
		if time.Now().After(deadline) {
			msg := "sensor loop did not release the bus within 90s"
			return nil, nil, &msg
		}
		time.Sleep(150 * time.Millisecond)
	}
	defer s.hwMu.Unlock()
	// Hardware TWI first; GPIO pairs only when no TWI hit.
	hits := ScanTwi()
	var pairErrs []string
	if len(hits) == 0 {
		for _, c := range cands {
			found, perr := ScanGpioPair(c[0], c[1])
			if perr != nil {
				pairErrs = append(pairErrs, *perr)
			}
			hits = append(hits, found...)
		}
	}
	return hits, pairErrs, nil
}

// PersistTarget writes the overlay file (best-effort).
func (s *Service) PersistTarget(t SensorTarget) error {
	if s.persistPath == "" {
		return fmt.Errorf("thermal: persistence disabled")
	}
	text := fmt.Sprintf("# mibee-eye thermal hardware overlay -- written by PUT /api/hardware/config\nthermal:\n  bus: %s\n  addr: 0x%02X\n",
		BusYAML(&t.Bus), t.Addr)
	return os.WriteFile(s.persistPath, []byte(text), 0o600)
}

// PersistPath exposes where the overlay lands (diagnostics).
func (s *Service) PersistPath() string { return s.persistPath }

// LoadOverlay reads the boot overlay (sibling thermal-hardware.yaml of the
// config file); nil when absent or malformed (logged).
func LoadOverlay(path string) *SensorTarget {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		Thermal struct {
			Bus  BusSpec `yaml:"bus"`
			Addr uint8   `yaml:"addr"`
		} `yaml:"thermal"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		slog.Warn("thermal: ignoring bad hardware overlay", "path", path, "error", err)
		return nil
	}
	return &SensorTarget{Bus: doc.Thermal.Bus, Addr: doc.Thermal.Addr}
}

// SSEPayloadJSON renders the no-grid payload for the events pump.
func (s *Service) SSEPayloadJSON() []byte {
	b, _ := json.Marshal(s.Payload(false))
	return b
}
