package web

import (
	"testing"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/h264"
)

func au(key bool) h264.AccessUnit {
	return h264.AccessUnit{KeyFrame: key, NALUs: []h264.NALU{{Data: []byte{0x1}}}}
}

// TestRealignTracker_NoLoss lets every unit through when nothing was lost.
func TestRealignTracker_NoLoss(t *testing.T) {
	rt := realignTracker{seenDrops: 0}
	for _, key := range []bool{true, false, false, true, false} {
		if !rt.allow(au(key), 0, false) {
			t.Fatalf("unit (key=%v) must pass when no loss occurred", key)
		}
	}
}

// TestRealignTracker_DropDetected blocks non-key units after the subscriber's
// drop counter advances and clears on the next keyframe.
func TestRealignTracker_DropDetected(t *testing.T) {
	rt := realignTracker{seenDrops: 2}
	if !rt.allow(au(true), 2, false) {
		t.Fatal("keyframe must pass before any loss")
	}
	if rt.allow(au(false), 3, false) {
		t.Fatal("non-key unit must be blocked after a drop")
	}
	if rt.allow(au(false), 3, false) {
		t.Fatal("subsequent non-key units must stay blocked")
	}
	if !rt.allow(au(true), 3, false) {
		t.Fatal("keyframe must pass and realign the stream")
	}
	if !rt.allow(au(false), 3, false) {
		t.Fatal("after realignment normal units pass again")
	}
}

// TestRealignTracker_DrainedBacklog treats a fast-forward that lands on a
// non-keyframe as a hole, but a drain landing on a keyframe as clean.
func TestRealignTracker_DrainedBacklog(t *testing.T) {
	rt := realignTracker{seenDrops: 0}
	if rt.allow(au(false), 0, true) {
		t.Fatal("drained backlog landing on non-key must block")
	}
	if rt.allow(au(false), 0, false) {
		t.Fatal("must stay blocked until a keyframe")
	}
	if !rt.allow(au(true), 0, false) {
		t.Fatal("keyframe must clear the hole")
	}
	if !rt.allow(au(true), 0, true) {
		t.Fatal("drain landing on a keyframe is a clean jump, must pass")
	}
	if !rt.allow(au(false), 0, false) {
		t.Fatal("normal flow after clean jump")
	}
}

// TestRealignTracker_DropBeforeKeyKeepsState: a second drop while already
// waiting for a keyframe must not lose the waiting state.
func TestRealignTracker_DropWhileWaiting(t *testing.T) {
	rt := realignTracker{seenDrops: 0}
	if rt.allow(au(false), 1, false) {
		t.Fatal("must block after first drop")
	}
	if rt.allow(au(false), 2, false) {
		t.Fatal("must stay blocked across a second drop")
	}
	if !rt.allow(au(true), 2, false) {
		t.Fatal("keyframe must realign")
	}
}
