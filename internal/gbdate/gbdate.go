// Package gbdate observes the platform clock carried on the SIP Date
// header of REGISTER responses (GB/T 28181-2022 §9.10.2).
//
// The gb28181-go device server exposes the last parsed value
// (Server.PlatformDateUnix); this package evaluates drift against the
// local clock with a three-state latch. Observation only — the system
// clock is never adjusted here; disciplining it stays a host/NTP
// decision (same posture as the rs/notebook products).
package gbdate

import "time"

// WarnDriftSecs is the drift magnitude worth an operator's attention.
const WarnDriftSecs int64 = 5

// Outcome of evaluating one platform-clock sample against the latch.
type Outcome int

const (
	// Stable: within threshold (never warned), or beyond it but not
	// moved another threshold since the last Warn — stay quiet.
	Stable Outcome = iota
	// Warn: beyond threshold and moved since the last Warn — log now,
	// latch the magnitude.
	Warn
	// Recovered: back within threshold after a Warn — log once, clear.
	Recovered
)

// LastWarned carries the latched |drift| of the last Warn; 0 = none.
type LastWarned int64

// Evaluate returns the outcome and the new latch. Signed drift is
// local-platform (how far local runs ahead). A naive bool-as-latch
// misjudges a stable drift (7s → 7s) as recovery and alternates
// warn/clear every poll — the three states exist for exactly that.
func Evaluate(platformUnix, localUnix int64, last LastWarned) (Outcome, int64, LastWarned) {
	drift := localUnix - platformUnix
	abs := drift
	if abs < 0 {
		abs = -drift
	}
	if abs <= WarnDriftSecs {
		if last != 0 {
			return Recovered, drift, 0
		}
		return Stable, drift, last
	}
	moved := abs - int64(last)
	if moved < 0 {
		moved = -moved
	}
	if last != 0 && moved < WarnDriftSecs {
		return Stable, drift, last
	}
	return Warn, drift, LastWarned(abs)
}

// Ticker cadence for the observer loop in the product wiring.
const ObserveInterval = 60 * time.Second
