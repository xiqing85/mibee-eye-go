package camera

import (
	"bytes"
	"strings"
	"testing"
)

func TestIsTelemetryLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"frame stats", "#12824039 (15.00 fps) exp 23156.00 ag 2.00 dg 1.00", true},
		{"single digit frame", "#7 (30.00 fps) exp 1000.00 ag 2.00 dg 1.00", true},
		{"hash only", "# comment", false},
		{"hash no paren", "#12824039 fps", false},
		{"libcamera error", "*** ERROR *** Camera has stopped", false},
		{"plain message", "Preview window blank", false},
		{"empty", "", false},
		{"no hash", "12824039 (15.00 fps)", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTelemetryLine([]byte(tt.line)); got != tt.want {
				t.Errorf("isTelemetryLine(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

func TestTelemetryFilterDropsStatsPassesRest(t *testing.T) {
	var out bytes.Buffer
	f := newTelemetryFilter(&out)

	in := "#100 (15.00 fps) exp 1.00 ag 2.00 dg 1.00\n" +
		"Preview window blank\n" +
		"#101 (15.00 fps) exp 1.00 ag 2.00 dg 1.00\n" +
		"*** ERROR *** Camera has stopped\n"
	n, err := f.Write([]byte(in))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(in) {
		t.Fatalf("write returned %d, want %d", n, len(in))
	}

	want := "Preview window blank\n*** ERROR *** Camera has stopped\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestTelemetryFilterHandlesSplitWrites(t *testing.T) {
	var out bytes.Buffer
	f := newTelemetryFilter(&out)

	// Telemetry line split across three writes must still be dropped.
	for _, chunk := range []string{"#12", "824039 (15.0", "0 fps) exp 1.00\n"} {
		if _, err := f.Write([]byte(chunk)); err != nil {
			t.Fatalf("write %q: %v", chunk, err)
		}
	}
	// A real message split across two writes must pass through intact.
	for _, chunk := range []string{"Made sel", "ection...\n"} {
		if _, err := f.Write([]byte(chunk)); err != nil {
			t.Fatalf("write %q: %v", chunk, err)
		}
	}

	want := "Made selection...\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestTelemetryFilterTrailingPartialLineBuffered(t *testing.T) {
	var out bytes.Buffer
	f := newTelemetryFilter(&out)

	// A trailing partial line stays buffered (no output yet)...
	if _, err := f.Write([]byte("partia")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want empty", out.String())
	}
	// ...and is emitted once its newline arrives.
	if _, err := f.Write([]byte("l line\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(out.String(), "partial line\n") {
		t.Errorf("output = %q, want it to contain %q", out.String(), "partial line\n")
	}
}
