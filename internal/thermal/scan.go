package thermal

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/xiqing85/mibee-eye-go/internal/i2c"
)

// AddrRange is the plausible MLX90640 address window (A1/A0 straps move
// the 7-bit slave address across 0x30..=0x3E; 0x33 is factory default).
const (
	AddrMin uint8 = 0x30
	AddrMax uint8 = 0x3E
)

func addrInRange(a uint8) bool { return a >= AddrMin && a <= AddrMax }

// Hit is one discovered sensor.
type Hit struct {
	Bus  string `json:"bus"`
	Addr uint8  `json:"addr"`
	ID   uint16 `json:"id"`
}

func plausibleID(id uint16) bool { return id != 0x0000 && id != 0xFFFF }

// TwiDevices enumerates /dev/i2c-* nodes present on this board.
func TwiDevices() []string {
	var out []string
	for n := 0; n < 16; n++ {
		p := fmt.Sprintf("/dev/i2c-%d", n)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// ScanTwi probes every hardware bus × the address window (read-only: one
// EE ID word per combination; unrelated chips NAK or return junk IDs).
func ScanTwi() []Hit {
	var hits []Hit
	for _, dev := range TwiDevices() {
		for addr := AddrMin; ; addr++ {
			bus, err := i2c.OpenTwi(dev, addr)
			if err != nil {
				break // bus unusable — skip its whole range
			}
			if id, err := NewMlx90640(bus).ReadID(); err == nil && plausibleID(id) {
				hits = append(hits, Hit{Bus: "twi:" + dev, Addr: addr, ID: id})
			}
			bus.Close()
			if addr == AddrMax {
				break
			}
		}
	}
	return hits
}

// ReleaseGpioPair releases the board's TWI controller + stale sysfs
// exports so bit-bang can own the candidate pins (idempotent,
// best-effort). The mv64xxx unbind below is the Orange Pi Zero 3 TWI2 on
// PH2/PH3 recipe — a no-op on other boards (write fails, ignored).
func ReleaseGpioPair(sda, scl uint32) {
	_ = os.WriteFile("/sys/bus/platform/drivers/mv64xxx_i2c/unbind", []byte("5002800.i2c"), 0o200)
	for _, n := range []uint32{sda, scl} {
		_ = os.WriteFile("/sys/class/gpio/unexport", []byte(fmt.Sprintf("%d", n)), 0o200)
	}
	time.Sleep(300 * time.Millisecond)
}

// PairErrorHint appends a one-line fix for the common probe failures —
// they must not be silent.
func PairErrorHint(err error) string {
	s := err.Error()
	switch {
	case contains(s, "Permission denied"):
		return " (sysfs GPIO export needs root)"
	case contains(s, "Device or resource busy"):
		return " (pins held by another process)"
	default:
		return ""
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ScanGpioPair probes one sysfs GPIO pair × the address window via
// bit-bang. Returns hits plus a failure reason when the pair itself was
// unusable (export denied, lines stuck) — distinct from "nothing there".
// One exported pair walks the whole window (BitBang.SetAddr).
func ScanGpioPair(sda, scl uint32) ([]Hit, *string) {
	var hits []Hit
	ReleaseGpioPair(sda, scl)
	bus, err := i2c.NewBitBang(sda, scl, AddrMin)
	if err != nil {
		msg := fmt.Sprintf("gpio %d,%d: %v%s", sda, scl, err, PairErrorHint(err))
		return hits, &msg
	}
	defer bus.Close()
	for addr := AddrMin; ; addr++ {
		bus.SetAddr(addr)
		if id, err := bus.ReadReg16(RegID); err == nil && plausibleID(id) {
			hits = append(hits, Hit{Bus: fmt.Sprintf("gpio:%d,%d", sda, scl), Addr: addr, ID: id})
		}
		if addr == AddrMax {
			break
		}
	}
	return hits, nil
}

// ConfigSnippet formats a hit as ready-to-paste config lines.
func ConfigSnippet(h *Hit) string {
	bus := hitBus(h)
	return fmt.Sprintf("thermal:\n  bus: %s\n  addr: 0x%02X\n", BusYAML(&bus), h.Addr)
}

// hitBus rebuilds the BusSpec a scan hit describes.
func hitBus(h *Hit) BusSpec {
	if dev, ok := trimPrefix(h.Bus, "twi:"); ok {
		return BusSpec{Type: "twi", Device: dev}
	}
	var sda, scl uint32
	fmt.Sscanf(h.Bus, "gpio:%d,%d", &sda, &scl)
	return BusSpec{Type: "gpio", Sda: sda, Scl: scl}
}

func trimPrefix(s, p string) (string, bool) {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):], true
	}
	return s, false
}

// AutoDetect resolves a working transport for boot-time bus auto:
// hardware TWI first (proper pull-ups), then the classic Orange Pi Zero 3
// PH2/PH3 pair as the bit-bang fallback.
func AutoDetect() (*Hit, bool) {
	if hits := ScanTwi(); len(hits) > 0 {
		return &hits[0], false
	}
	if hits, _ := ScanGpioPair(226, 227); len(hits) > 0 {
		return &hits[0], true
	}
	return nil, false
}

// scanDevicesOwner annotates a /dev/i2c node with its sysfs owner name —
// keeps scan logs honest about which controller is which.
func scanDevicesOwner(dev string) string {
	b, err := os.ReadFile(filepath.Join("/sys/class/i2c-dev",
		filepath.Base(dev), "name"))
	if err != nil {
		return "?"
	}
	return trimSpace(string(b))
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\r' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\r' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
