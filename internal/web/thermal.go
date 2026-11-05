package web

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/xiqing85/mibee-eye-go/internal/thermal"
)

// Thermal extension handlers (SPEC §3.7). Registered only when the side
// sensor subsystem runs (web.Config.Thermal != nil) and session-gated by
// authRequired like every other /api/* route.

func (s *Server) handleThermalCurrent(w http.ResponseWriter, r *http.Request) {
	grid := r.URL.Query().Get("grid") == "1"
	writeOK(w, http.StatusOK, s.cfg.Thermal.Payload(grid))
}

func (s *Server) handleThermalSnapshot(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("format") == "bmp" {
		var lo, hi *float32
		if v, err := strconv.ParseFloat(q.Get("lo"), 32); err == nil {
			lo = ptr32(float32(v))
		}
		if v, err := strconv.ParseFloat(q.Get("hi"), 32); err == nil {
			hi = ptr32(float32(v))
		}
		to := s.cfg.Thermal.Grid()
		var rgb []byte
		if q.Get("overlay") == "0" {
			rgb = thermal.RenderRGBRange(&to, 640, 480, lo, hi)
		} else {
			rgb = thermal.RenderRGBAnnotated(&to, 640, 480, lo, hi)
		}
		w.Header().Set("Content-Type", "image/bmp")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(thermal.RGBToBMP(rgb, 640, 480))
		return
	}
	writeOK(w, http.StatusOK, s.cfg.Thermal.Payload(true))
}

func ptr32(v float32) *float32 { return &v }

func (s *Server) handleHardwareGet(w http.ResponseWriter, r *http.Request) {
	t := s.cfg.Thermal.Target()
	writeOK(w, http.StatusOK, map[string]interface{}{
		"bus":          t.Bus,
		"addr":         t.Addr,
		"persist_path": s.cfg.Thermal.PersistPath(),
		"scan":         thermal.ScanJSON(s.cfg.Thermal.ScanState()),
	})
}

func (s *Server) handleHardwareScanGet(w http.ResponseWriter, r *http.Request) {
	writeOK(w, http.StatusOK, thermal.ScanJSON(s.cfg.Thermal.ScanState()))
}

func (s *Server) handleHardwareScanPost(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Thermal.ScanState().Running {
		writeError(w, http.StatusConflict, "scan already running")
		return
	}
	// Optional {"gpio_candidates":[[sda,scl],…]} body — default is the
	// classic Orange Pi Zero 3 PH2/PH3 pair (a Pi on TWI finds its sensor
	// in the TWI sweep before GPIO pairs are touched).
	cands := [][2]uint32{{226, 227}}
	var body struct {
		GpioCandidates [][2]uint32 `json:"gpio_candidates"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err == nil && len(body.GpioCandidates) > 0 {
		cands = body.GpioCandidates
	}
	if !s.cfg.Thermal.StartScan(cands) {
		writeError(w, http.StatusConflict, "scan already running")
		return
	}
	writeOK(w, http.StatusOK, map[string]interface{}{"running": true})
}

func (s *Server) handleHardwareConfigPut(w http.ResponseWriter, r *http.Request) {
	var put struct {
		Bus  thermal.BusSpec `json:"bus"`
		Addr uint8           `json:"addr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&put); err != nil {
		writeError(w, http.StatusBadRequest, "bad config: "+err.Error())
		return
	}
	if !addrInRange(put.Addr) {
		writeError(w, http.StatusBadRequest, "addr must be 0x30..=0x3E (MLX90640 strapping window)")
		return
	}
	if put.Bus.Type == "gpio" && (put.Bus.Sda == put.Bus.Scl || put.Bus.Sda == 0 || put.Bus.Scl == 0) {
		writeError(w, http.StatusBadRequest, "gpio sda/scl must be distinct non-zero pins")
		return
	}
	t := thermal.SensorTarget{Bus: put.Bus, Addr: put.Addr}
	s.cfg.Thermal.SetTarget(t)
	// Best-effort persistence — runtime apply already happened.
	persisted, persistErr := true, ""
	if err := s.cfg.Thermal.PersistTarget(t); err != nil {
		persisted, persistErr = false, err.Error()
	}
	var persistErrPtr *string
	if !persisted {
		persistErrPtr = &persistErr
	}
	writeOK(w, http.StatusOK, map[string]interface{}{
		"bus":           put.Bus,
		"addr":          put.Addr,
		"persisted":     persisted,
		"persist_error": persistErrPtr,
	})
}

// addrInRange mirrors thermal.AddrMin/AddrMax without exporting the
// window arithmetic into the web layer.
func addrInRange(a uint8) bool {
	return a >= thermal.AddrMin && a <= thermal.AddrMax
}
