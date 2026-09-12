// telemetry_filter.go — drops rpicam-vid per-frame telemetry from stderr.
//
// rpicam-vid prints one stats line per encoded frame to stderr, e.g.
//
//	#12824039 (15.00 fps) exp 23156.00 ag 2.00 dg 1.00
//
// At 15 fps that is ~130k lines/day (~67 MB) of noise funnelled into
// run.log/journald. telemetryFilter drops exactly those lines and passes
// everything else (real libcamera/rpicam errors included) through.
package camera

import (
	"bytes"
	"io"
	"sync"
)

// isTelemetryLine reports whether line is a rpicam-vid per-frame stats
// line: `#` followed by decimal digits and " (".
func isTelemetryLine(line []byte) bool {
	i := 1
	if len(line) == 0 || line[0] != '#' {
		return false
	}
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	return bytes.HasPrefix(line[i:], []byte(" ("))
}

// telemetryFilter is an io.Writer that drops rpicam-vid per-frame
// telemetry lines and forwards every other complete line to dst.
//
// Writes may split lines at arbitrary byte boundaries; a partial line is
// buffered until its newline arrives. Whatever is still buffered when the
// subprocess exits is dropped — in practice only a trailing partial
// telemetry line, never a completed message.
type telemetryFilter struct {
	mu  sync.Mutex
	dst io.Writer
	buf []byte
}

// newTelemetryFilter wraps dst so rpicam-vid telemetry lines are dropped.
func newTelemetryFilter(dst io.Writer) *telemetryFilter {
	return &telemetryFilter{dst: dst}
}

// Write implements io.Writer.
func (f *telemetryFilter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			break
		}
		line := f.buf[:i]
		f.buf = f.buf[i+1:]
		if isTelemetryLine(line) {
			continue
		}
		if _, err := f.dst.Write(line); err != nil {
			return len(p), err
		}
		if _, err := f.dst.Write([]byte{'\n'}); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}
