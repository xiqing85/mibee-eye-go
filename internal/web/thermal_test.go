package web

import (
	"encoding/json"
	"testing"

	"github.com/xiqing85/mibee-eye-go/internal/thermal"
)

// Negative compat: without a thermal subsystem the endpoints stay
// unregistered (mux 404 — never the SPA) and the capability key absent.
func TestThermalEndpointsNilGated(t *testing.T) {
	s := newSpecServer("admin", "spec-pass-1")
	ck, csrf := specLogin(t, s)
	hdr := authHdr(ck, csrf)

	rec := doReq(t, s, "GET", "/api/thermal/current", "", hdr)
	if rec.Code != httpStatusNotFound {
		t.Errorf("thermal/current without subsystem = %d, want 404", rec.Code)
	}
	rec = doReq(t, s, "GET", "/api/hardware", "", hdr)
	if rec.Code != httpStatusNotFound {
		t.Errorf("hardware without subsystem = %d, want 404", rec.Code)
	}

	rec = doReq(t, s, "GET", "/api/capabilities", "", hdr)
	var doc struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, has := doc.Data["thermal"]; has {
		t.Error("thermal capability must be absent without the subsystem")
	}
}

// With the subsystem running: envelope payload carries the honest warmup
// state (trusted:false + reason) and the capability key advertises it.
func TestThermalEndpointsPresent(t *testing.T) {
	svc := thermal.NewService(&thermal.Config{
		Enabled: true, Addr: 0x33, Emissivity: 1.0,
	})
	s := New(Config{
		Port:        8088,
		Username:    "admin",
		Password:    "spec-pass-1",
		OnvifConfig: &mockOnvifConfig{port: 8080, username: "o", password: "p"},
		Version:     "test",
		Thermal:     svc,
	})
	ck, csrf := specLogin(t, s)
	hdr := authHdr(ck, csrf)

	rec := doReq(t, s, "GET", "/api/thermal/current?grid=1", "", hdr)
	if rec.Code != 200 {
		t.Fatalf("current = %d", rec.Code)
	}
	var doc struct {
		OK   bool `json:"ok"`
		Data struct {
			Trusted bool           `json:"trusted"`
			Reason  string         `json:"reason"`
			Grid    map[string]any `json:"grid"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Data.Trusted {
		t.Error("warmup state must be untrusted")
	}
	if doc.Data.Reason == "" {
		t.Error("untrusted payload must carry a reason")
	}
	vals, _ := doc.Data.Grid["values"].([]any)
	if len(vals) != 768 {
		t.Errorf("grid values = %d, want 768", len(vals))
	}

	// Snapshot BMP renders even in warmup (flat grid).
	rec = doReq(t, s, "GET", "/api/thermal/snapshot?format=bmp", "", hdr)
	if rec.Code != 200 {
		t.Fatalf("snapshot = %d", rec.Code)
	}
	if b := rec.Body.Bytes(); len(b) < 54 || string(b[:2]) != "BM" {
		t.Errorf("snapshot not a BMP: %d bytes", len(b))
	}

	// Hardware: target round-trips through the API.
	rec = doReq(t, s, "PUT", "/api/hardware/config",
		`{"bus":{"type":"gpio","sda":2,"scl":3},"addr":51}`, hdr)
	if rec.Code != 200 {
		t.Fatalf("hardware put = %d: %s", rec.Code, rec.Body.String())
	}
	if got := svc.Target(); got.Bus.Type != "gpio" || got.Bus.Sda != 2 || got.Addr != 0x33 {
		t.Errorf("target = %+v", got)
	}
	// Addr window enforced.
	rec = doReq(t, s, "PUT", "/api/hardware/config",
		`{"bus":{"type":"auto"},"addr":100}`, hdr)
	if rec.Code != 400 {
		t.Errorf("addr 100 = %d, want 400", rec.Code)
	}

	rec = doReq(t, s, "GET", "/api/capabilities", "", hdr)
	var caps struct {
		Data struct {
			Thermal map[string]any `json:"thermal"`
			Events  []string       `json:"events"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	if caps.Data.Thermal["hardware"] != true {
		t.Errorf("thermal cap = %+v", caps.Data.Thermal)
	}
	found := false
	for _, e := range caps.Data.Events {
		if e == "thermal.temperature" {
			found = true
		}
	}
	if !found {
		t.Error("thermal.temperature not advertised in events")
	}
}

const httpStatusNotFound = 404
