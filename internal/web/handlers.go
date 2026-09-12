package web

// Config and imaging endpoints (SPEC v1 §5, §4.5).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/camera"

	"gopkg.in/yaml.v3"
)

// handleGetConfig returns the full configuration dump (enveloped, secrets
// masked to "****" per SPEC §5).
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	oc := s.cfg.OnvifConfig
	if oc == nil {
		writeError(w, http.StatusInternalServerError, "onvif config not available")
		return
	}

	cameraSection := map[string]interface{}{
		"device":  oc.CameraDevice(),
		"width":   oc.CameraWidth(),
		"height":  oc.CameraHeight(),
		"fps":     oc.CameraFPS(),
		"codec":   oc.CameraCodec(),
		"bitrate": oc.CameraBitrate(),
		// Persistent device-level flips (YAML camera.hflip/vflip). Live
		// PascalCase params are deliberately NOT overlaid here — the editor
		// round-trip would write them back as duplicate keys; the imaging
		// endpoints (SPEC §4.5) are their API surface.
		"hflip": oc.CameraHFlip(),
		"vflip": oc.CameraVFlip(),
	}

	webUser, webPass := s.currentCredentials()
	config := map[string]interface{}{
		"camera": cameraSection,
		"rtsp": map[string]interface{}{
			"port":     oc.RTSPPort(),
			"username": "",
			"password": "",
		},
		"onvif": map[string]interface{}{
			"port":     oc.ONVIFPort(),
			"username": oc.ONVIFUsername(),
			"password": maskPassword(oc.ONVIFPassword()),
		},
		"device": map[string]interface{}{
			"name":          oc.DeviceName(),
			"manufacturer":  oc.DeviceManufacturer(),
			"model":         oc.DeviceModel(),
			"firmware":      oc.DeviceFirmware(),
			"hardware_id":   oc.DeviceHardwareID(),
			"serial_number": oc.DeviceSerialNumber(),
		},
		"logging": map[string]interface{}{
			"level": oc.LoggingLevel(),
		},
		"web": map[string]interface{}{
			"enabled":  true,
			"port":     s.cfg.Port,
			"username": webUser,
			"password": maskPassword(webPass),
		},
		"gb28181": func() map[string]interface{} {
			if s.cfg.GB28181Config == nil {
				return map[string]interface{}{"enabled": false}
			}
			return map[string]interface{}{
				"enabled":                 s.cfg.GB28181Config.Enabled,
				"platform_sip_address":    s.cfg.GB28181Config.PlatformSIPAddress,
				"platform_sip_port":       s.cfg.GB28181Config.PlatformSIPPort,
				"device_id":               s.cfg.GB28181Config.DeviceID,
				"channel_id":              s.cfg.GB28181Config.ChannelID,
				"sip_domain":              s.cfg.GB28181Config.SIPDomain,
				"password":                maskPassword(s.cfg.GB28181Config.Password),
				"local_sip_port":          s.cfg.GB28181Config.LocalSIPPort,
				"register_interval_secs":  s.cfg.GB28181Config.RegisterIntervalSecs,
				"heartbeat_interval_secs": s.cfg.GB28181Config.HeartbeatIntervalSecs,
				"heartbeat_timeout_count": s.cfg.GB28181Config.HeartbeatTimeoutCount,
			}
		}(),
	}

	writeOK(w, http.StatusOK, config)
}

// handlePutConfig accepts a partial config document (SPEC §5), deep-merges it
// over the YAML file, restores masked secrets, and restarts the process so
// the change applies (config_apply default = restart). If the web section
// changes the credentials, the in-memory ones update immediately too.
func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ConfigPath == "" {
		writeError(w, http.StatusNotImplemented, "config path not configured")
		return
	}

	var update map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	data, err := os.ReadFile(s.cfg.ConfigPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to read config: %v", err))
		return
	}
	var cfg map[string]interface{}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to parse config: %v", err))
		return
	}

	restoreMaskedSecrets(update, cfg)
	deepMerge(cfg, update)

	out, err := yaml.Marshal(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to marshal config: %v", err))
		return
	}
	if err := atomicWrite(s.cfg.ConfigPath, out); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Credentials take effect immediately so a fresh login works post-restart.
	if webSec, ok := update["web"].(map[string]interface{}); ok {
		if u, ok := webSec["username"].(string); ok && u != "" && u != "****" {
			s.mu.Lock()
			s.username = u
			s.mu.Unlock()
		}
	}

	s.logger.Printf("web: config updated, restarting in 500ms")
	s.selfRestart()

	writeOK(w, http.StatusOK, map[string]interface{}{"applied": "restart"})
}

// handleSystemRestart implements POST /api/system/restart (SPEC §5.1): apply
// all saved restart-semantics config by restarting the service process.
func (s *Server) handleSystemRestart(w http.ResponseWriter, r *http.Request) {
	s.logger.Printf("web: restart requested via /api/system/restart")
	s.selfRestart()
	writeOK(w, http.StatusOK, map[string]interface{}{"status": "restarting"})
}

// restoreMaskedSecrets replaces "****" password values in the update with
// the stored ones so a masked GET → PUT round-trip keeps secrets.
func restoreMaskedSecrets(update, current map[string]interface{}) {
	sections := []string{"onvif", "web", "gb28181", "rtsp"}
	for _, sec := range sections {
		upd, ok := update[sec].(map[string]interface{})
		if !ok {
			continue
		}
		pw, ok := upd["password"].(string)
		if !ok || pw != "****" {
			continue
		}
		if cur, ok := current[sec].(map[string]interface{}); ok {
			if stored, ok := cur["password"].(string); ok {
				upd["password"] = stored
			}
		}
	}
}

// deepMerge recursively merges src into dst (maps only; other values replace).
func deepMerge(dst, src map[string]interface{}) {
	for k, v := range src {
		if sub, ok := v.(map[string]interface{}); ok {
			if dsub, ok := dst[k].(map[string]interface{}); ok {
				deepMerge(dsub, sub)
				continue
			}
		}
		dst[k] = v
	}
}

// atomicWrite writes data to path via a temp file + rename.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to sync temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename config: %w", err)
	}
	return nil
}

// handleGetCameraParams returns all current imaging parameters (SPEC §4.5).
func (s *Server) handleGetCameraParams(w http.ResponseWriter, r *http.Request) {
	if !s.requireCamera0(w, r) {
		return
	}
	if s.cfg.Params == nil {
		writeError(w, http.StatusInternalServerError, "param manager not available")
		return
	}

	result := map[string]interface{}{}
	for name := range camera.ParamRanges {
		if val, err := s.cfg.Params.Get(name); err == nil {
			result[name] = val
		}
	}
	for name := range camera.ParamEnums {
		if val, err := s.cfg.Params.Get(name); err == nil {
			result[name] = val
		}
	}
	writeOK(w, http.StatusOK, result)
}

// handlePostCameraParam sets a single imaging parameter (SPEC §4.5).
// HFlip/VFlip are the exception to "immediate effect": rpicam-vid has no
// runtime flip channel, so they are persisted into the YAML camera section
// and the service restarts to bake them into the pipeline
// (附录A #9; response carries applied:"restart").
func (s *Server) handlePostCameraParam(w http.ResponseWriter, r *http.Request) {
	if !s.requireCamera0(w, r) {
		return
	}
	if s.cfg.Params == nil {
		writeError(w, http.StatusInternalServerError, "param manager not available")
		return
	}

	var req struct {
		Name  string      `json:"name"`
		Value interface{} `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "parameter name is required")
		return
	}

	value := coerceFloat64(req.Value)

	// Flip no-op short-circuit (附录A #9): a flip that already matches the
	// live pipeline has nothing to bake in — skip the YAML write and the
	// restart. resetDefaults POSTs both flips back-to-back with default
	// values; unchanged values must not stack restarts against the
	// systemd StartLimit.
	if (req.Name == "HFlip" || req.Name == "VFlip") && s.flipUnchanged(req.Name, value) {
		s.logger.Printf("web: camera param %s already %v, no restart needed", req.Name, value)
		writeOK(w, http.StatusOK, map[string]interface{}{"name": req.Name, "value": value})
		return
	}

	if err := s.cfg.Params.Set(req.Name, value); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp := map[string]interface{}{"name": req.Name, "value": value}
	if req.Name == "HFlip" || req.Name == "VFlip" {
		if err := s.persistCameraFlip(strings.ToLower(req.Name), value); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("persist flip: %v", err))
			return
		}
		s.logger.Printf("web: camera param %s set to %v, restarting to apply", req.Name, value)
		s.selfRestart()
		resp["applied"] = "restart"
	} else {
		s.logger.Printf("web: camera param %s set to %v", req.Name, value)
	}
	writeOK(w, http.StatusOK, resp)
}

// flipUnchanged reports whether the live pipeline already sits at the posted
// flip value. An unreadable or non-boolean current value counts as changed so
// the safe path (persist + restart) still runs.
func (s *Server) flipUnchanged(name string, value interface{}) bool {
	cur, err := s.cfg.Params.Get(name)
	if err != nil {
		return false
	}
	curBool, okCur := flipAsBool(cur)
	valBool, okVal := flipAsBool(value)
	return okCur && okVal && curBool == valBool
}

// flipAsBool normalizes bool / 0|1 numeric flip values for comparison.
func flipAsBool(v interface{}) (bool, bool) {
	switch b := v.(type) {
	case bool:
		return b, true
	case int:
		return b != 0, true
	case float64:
		return b != 0, true
	}
	return false, false
}

// persistCameraFlip merges hflip/vflip into the YAML camera section and
// atomically rewrites the file (same read-merge-write as handlePutConfig).
func (s *Server) persistCameraFlip(key string, value interface{}) error {
	if s.cfg.ConfigPath == "" {
		return fmt.Errorf("config path not configured")
	}
	data, err := os.ReadFile(s.cfg.ConfigPath)
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}
	var cfg map[string]interface{}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}
	camSec, ok := cfg["camera"].(map[string]interface{})
	if !ok {
		camSec = map[string]interface{}{}
		cfg["camera"] = camSec
	}
	camSec[key] = value
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	return atomicWrite(s.cfg.ConfigPath, out)
}

// handleGetCameraOptions returns imaging parameter ranges and enums.
func (s *Server) handleGetCameraOptions(w http.ResponseWriter, r *http.Request) {
	if !s.requireCamera0(w, r) {
		return
	}
	result := map[string]interface{}{}
	for name, rg := range camera.ParamRanges {
		result[name] = map[string]interface{}{
			"min":     rg.Min,
			"max":     rg.Max,
			"default": rg.Default,
		}
	}
	for name, enums := range camera.ParamEnums {
		result[name] = map[string]interface{}{"enums": enums}
	}
	writeOK(w, http.StatusOK, result)
}

// requireCamera0 answers 404 for any camera id other than "0".
func (s *Server) requireCamera0(w http.ResponseWriter, r *http.Request) bool {
	if r.PathValue("id") != "0" {
		writeError(w, http.StatusNotFound, "no such camera")
		return false
	}
	return true
}
