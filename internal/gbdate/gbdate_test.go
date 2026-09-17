package gbdate

import "testing"

// The four latch behaviors, mirroring the rs/notebook twins' tests.
func TestWithinThresholdIsQuiet(t *testing.T) {
	out, _, l := Evaluate(1_000, 1_000, 0)
	if out != Stable || l != 0 {
		t.Fatalf("want Stable/0, got %v/%d", out, l)
	}
	out, _, l = Evaluate(1_000, 1_005, 0)
	if out != Stable || l != 0 {
		t.Fatalf("want Stable/0 at exactly the threshold, got %v/%d", out, l)
	}
}

func TestFirstExcursionWarns(t *testing.T) {
	out, drift, l := Evaluate(1_000, 1_007, 0)
	if out != Warn || drift != 7 || l != 7 {
		t.Fatalf("want Warn/7/7, got %v/%d/%d", out, drift, l)
	}
	// Sign preserved: platform ahead of local.
	out, drift, _ = Evaluate(1_012, 1_000, 0)
	if out != Warn || drift != -12 {
		t.Fatalf("want Warn/-12 (platform ahead), got %v/%d", out, drift)
	}
}

func TestStableDriftDoesNotRewarn(t *testing.T) {
	// Already warned at 7s; the same drift (or a 1s wiggle) stays quiet.
	out, _, l := Evaluate(1_000, 1_007, 7)
	if out != Stable || l != 7 {
		t.Fatalf("want Stable/7 (latch kept), got %v/%d", out, l)
	}
	out, _, _ = Evaluate(1_000, 1_009, 7)
	if out != Stable {
		t.Fatalf("want Stable on a 2s wiggle, got %v", out)
	}
	// Moved another threshold → warn again.
	out, drift, l := Evaluate(1_000, 1_013, 7)
	if out != Warn || drift != 13 || l != 13 {
		t.Fatalf("want Warn/13/13 after moving another threshold, got %v/%d/%d", out, drift, l)
	}
}

func TestRecoveryClearsTheLatch(t *testing.T) {
	out, _, l := Evaluate(1_000, 1_003, 7)
	if out != Recovered || l != 0 {
		t.Fatalf("want Recovered/0, got %v/%d", out, l)
	}
	// Latch cleared → the next excursion warns immediately.
	out, _, _ = Evaluate(1_000, 1_006, 0)
	if out != Warn {
		t.Fatalf("want Warn after recovery, got %v", out)
	}
}
