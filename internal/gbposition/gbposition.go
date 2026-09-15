// Package gbposition reports a fixed camera's surveyed coordinates as
// GB/T 28181 MobilePosition NOTIFYs (§9.5.3) while a platform holds a
// position subscription. Coordinates come from config
// (gb28181.longitude / gb28181.latitude, GB 度分秒 string form, e.g.
// "1163942.55E" / "395436.30N") and are carried verbatim — formatting
// stays with the operator. Twin of mibee-eye-rs gb28181_position.
package gbposition

import (
	"time"

	gbdev "github.com/mickeyzzc/gb28181-go/device"
)

// StaticPosition reports fixed coordinates on the subscription cadence.
type StaticPosition struct {
	Longitude string
	Latitude  string
}

// CurrentPosition implements gbdev.PositionSource; a static camera
// always reports.
func (p *StaticPosition) CurrentPosition() *gbdev.PositionReport {
	return &gbdev.PositionReport{
		Time:      time.Now().Format("2006-01-02T15:04:05"),
		Longitude: p.Longitude,
		Latitude:  p.Latitude,
	}
}
