package ai

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDefaultIsRegistry320(t *testing.T) {
	m, ok := Resolve("", DefaultModelPath)
	if !ok {
		t.Fatal("empty model id must default to the 320 entry")
	}
	if m.ID != "nanodet-plus-m-320" || m.Custom || m.Family != "nanodet" || m.Input != 320 {
		t.Fatalf("m = %+v", m)
	}
	if m.Path != "/var/lib/mibee-eye/models/nanodet-m.onnx" {
		t.Fatalf("path = %s", m.Path)
	}
}

func TestResolve416Entry(t *testing.T) {
	m, ok := Resolve("nanodet-plus-m-416", DefaultModelPath)
	if !ok {
		t.Fatal("registry id must resolve")
	}
	if m.Input != 416 || m.Path != "/var/lib/mibee-eye/models/nanodet-m-416.onnx" {
		t.Fatalf("m = %+v", m)
	}
}

func TestResolveCustomModelPathWins(t *testing.T) {
	m, ok := Resolve("nanodet-plus-m-320", "/opt/my-nanodet.onnx")
	if !ok {
		t.Fatal("custom path must resolve")
	}
	if !m.Custom || m.ID != "custom" || m.Path != "/opt/my-nanodet.onnx" {
		t.Fatalf("m = %+v", m)
	}
}

func TestResolveUnknownIDRejected(t *testing.T) {
	if _, ok := Resolve("yolo-9000", DefaultModelPath); ok {
		t.Fatal("unknown id must be rejected")
	}
}

func TestRegistryIDsUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range RegistryList() {
		if seen[m.ID] {
			t.Fatalf("duplicate id %s", m.ID)
		}
		seen[m.ID] = true
	}
}

func TestUploadedManifestRoundtrip(t *testing.T) {
	dir := t.TempDir()
	InitRegistry(dir)
	t.Cleanup(func() { InitRegistry("") })
	if len(RegistryList()) != len(builtinModels()) {
		t.Fatalf("fresh registry = %d entries", len(RegistryList()))
	}

	f := filepath.Join(dir, "custom-model.onnx")
	if err := os.WriteFile(f, []byte("onnx"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RegisterUploaded(ModelSpec{
		ID: "custom-model", Family: "yolox", Input: 416,
		Path: f, Source: "uploaded",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, ok := Find("custom-model"); !ok {
		t.Fatal("entry must be findable")
	}

	// Reload picks the manifest up.
	InitRegistry(dir)
	m, ok := Find("custom-model")
	if !ok || m.Source != "uploaded" || m.Input != 416 {
		t.Fatalf("reloaded entry = %+v ok=%v", m, ok)
	}

	// Vanished file prunes on reload.
	os.Remove(f)
	InitRegistry(dir)
	if _, ok := Find("custom-model"); ok {
		t.Fatal("vanished entry must be pruned")
	}

	// Remove refuses builtin.
	if RemoveUploaded("nanodet-plus-m-320") != nil {
		t.Fatal("builtin removal must be refused")
	}
}

func TestValidModelID(t *testing.T) {
	for _, ok := range []string{"nanodet-plus-m-320", "my-model-1", "a"} {
		if !ValidModelID(ok) {
			t.Errorf("%q must be valid", ok)
		}
	}
	for _, bad := range []string{"", "-leading", "Upper", "has_underscore", "has space", string(make([]byte, 65))} {
		if ValidModelID(bad) {
			t.Errorf("%q must be invalid", bad)
		}
	}
}
