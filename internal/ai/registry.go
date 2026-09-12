package ai

// Built-in model registry (SPEC v1 §4.6): stable model ids → on-device ONNX
// files plus the metadata GET /api/ai/models reports. Entries must stay
// within a decoder family the pre/post-processing implements (today:
// NanoDet GFL); see docs before adding a new family.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ModelSpec is one entry of the registry.
type ModelSpec struct {
	ID     string // registry id — the "model" value of SPEC §4.6
	Family string // decoder family; selects the pre/post-processing pair
	Input  int    // square input size in pixels (informational)
	Path   string // on-device ONNX file
	Source string // "builtin" | "uploaded"
}

// ModelsDir is the canonical model directory (uploads + manifest).
const ModelsDir = "/var/lib/mibee-eye/models"

// UploadMaxBytes is the upload size cap (SPEC §4.6 max_bytes).
const UploadMaxBytes = 32 << 20

// uploadEntry is one manifest record (persisted as uploaded.json).
type uploadEntry struct {
	ID     string `json:"id"`
	Family string `json:"family"`
	Input  int    `json:"input"`
	File   string `json:"file"`
}

// The runtime registry: builtin entries plus the uploaded overlay,
// guarded for concurrent web access.
var (
	regMu      sync.RWMutex
	regEntries []ModelSpec
	regDir     string
)

func init() { regEntries = builtinModels() }

func builtinModels() []ModelSpec {
	return []ModelSpec{
		{ID: "nanodet-plus-m-320", Family: "nanodet", Input: 320, Path: ModelsDir + "/nanodet-m.onnx", Source: "builtin"},
		{ID: "nanodet-plus-m-416", Family: "nanodet", Input: 416, Path: ModelsDir + "/nanodet-m-416.onnx", Source: "builtin"},
		{ID: "yolox-nano-416", Family: "yolox", Input: 416, Path: ModelsDir + "/yolox-nano.onnx", Source: "builtin"},
	}
}

// InitRegistry loads builtin entries plus the uploaded manifest from dir.
// Manifest entries whose files vanished are pruned. Call once at startup
// (and from tests needing persistence).
func InitRegistry(dir string) {
	regMu.Lock()
	defer regMu.Unlock()
	regDir = dir
	regEntries = builtinModels()
	if dir == "" {
		return
	}
	raw, err := os.ReadFile(filepath.Join(dir, "uploaded.json"))
	if err != nil {
		return
	}
	var uploaded []uploadEntry
	if json.Unmarshal(raw, &uploaded) != nil {
		return
	}
	kept := uploaded[:0]
	for _, e := range uploaded {
		if _, err := os.Stat(filepath.Join(dir, e.File)); err != nil {
			continue
		}
		kept = append(kept, e)
		regEntries = append(regEntries, ModelSpec{
			ID: e.ID, Family: e.Family, Input: e.Input,
			Path: filepath.Join(dir, e.File), Source: "uploaded",
		})
	}
	if len(kept) != len(uploaded) {
		writeManifest(dir, kept)
	}
}

// ModelsDir returns the registry's models directory ("" = no persistence).
func RegistryDir() string {
	regMu.RLock()
	defer regMu.RUnlock()
	return regDir
}

// SetRegistryEntries replaces the entry list (test hook for pointing
// builtin paths at temp files).
func SetRegistryEntries(entries []ModelSpec) {
	regMu.Lock()
	defer regMu.Unlock()
	regEntries = entries
}

// RegistryList returns all entries (builtin first, then uploaded).
func RegistryList() []ModelSpec {
	regMu.RLock()
	defer regMu.RUnlock()
	return append([]ModelSpec(nil), regEntries...)
}

// RegisterUploaded adds a validated entry and persists the manifest.
func RegisterUploaded(spec ModelSpec) error {
	regMu.Lock()
	defer regMu.Unlock()
	if regDir == "" {
		return fmt.Errorf("registry has no models dir")
	}
	uploaded := currentUploads()
	uploaded = append(uploaded, uploadEntry{
		ID: spec.ID, Family: spec.Family, Input: spec.Input,
		File: filepath.Base(spec.Path),
	})
	if err := writeManifest(regDir, uploaded); err != nil {
		return err
	}
	regEntries = append(regEntries, spec)
	return nil
}

// RemoveUploaded deletes an uploaded entry (persisting the manifest) and
// returns it; nil when unknown or not uploaded.
func RemoveUploaded(id string) *ModelSpec {
	regMu.Lock()
	defer regMu.Unlock()
	for i, m := range regEntries {
		if m.ID == id && m.Source == "uploaded" {
			regEntries = append(regEntries[:i], regEntries[i+1:]...)
			if regDir != "" {
				writeManifest(regDir, currentUploads())
			}
			spec := m
			return &spec
		}
	}
	return nil
}

func currentUploads() []uploadEntry {
	out := []uploadEntry{}
	for _, m := range regEntries {
		if m.Source != "uploaded" {
			continue
		}
		out = append(out, uploadEntry{
			ID: m.ID, Family: m.Family, Input: m.Input,
			File: filepath.Base(m.Path),
		})
	}
	return out
}

func writeManifest(dir string, uploaded []uploadEntry) error {
	raw, err := json.MarshalIndent(uploaded, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "uploaded.json"), raw, 0o644)
}

// family selects the pre/post-processing pair for a model.
type family int

const (
	familyNanoDet family = iota
	familyYolox
)

// familyOf maps a registry family string; unknown values fall back to
// NanoDet (the escape hatch predates families).
func familyOf(s string) family {
	if s == "yolox" {
		return familyYolox
	}
	return familyNanoDet
}

// DefaultModelPath is the registry default for model_path overrides.
const DefaultModelPath = "/var/lib/mibee-eye/models/nanodet-m.onnx"

// Activation errors surfaced by ActivateModel (mapped to HTTP 404/409).
var (
	ErrUnknownModel     = errors.New("unknown model id")
	ErrModelUnavailable = errors.New("model file not available")
)

// ActiveModel is what a configuration resolves to.
type ActiveModel struct {
	ID     string
	Path   string
	Family string
	Input  int
	Custom bool
}

// Find looks up a registry entry by id (builtin + uploaded).
func Find(id string) (ModelSpec, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	for _, m := range regEntries {
		if m.ID == id {
			return m, true
		}
	}
	return ModelSpec{}, false
}

// ValidModelID checks the SPEC §4.6 id syntax.
func ValidModelID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	if !(id[0] >= 'a' && id[0] <= 'z' || id[0] >= '0' && id[0] <= '9') {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Available reports whether the model file exists on this device (drives
// the "available" field of GET /api/ai/models).
func Available(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Resolve maps (model id, model path) to the model that should be loaded.
// A model_path differing from DefaultModelPath is a custom deployment
// override and wins over the registry id. Returns ok=false for an unknown
// id with the default path (callers treat that as invalid configuration).
func Resolve(model, modelPath string) (ActiveModel, bool) {
	if modelPath != "" && modelPath != DefaultModelPath {
		return ActiveModel{
			ID:     "custom",
			Path:   modelPath,
			Family: "nanodet", // the decode path only implements NanoDet
			Custom: true,
		}, true
	}
	if model == "" {
		model = "nanodet-plus-m-320"
	}
	regMu.RLock()
	defer regMu.RUnlock()
	for _, m := range regEntries {
		if m.ID == model {
			return ActiveModel{ID: m.ID, Path: m.Path, Family: m.Family, Input: m.Input}, true
		}
	}
	return ActiveModel{}, false
}
