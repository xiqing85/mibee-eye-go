package gbalarm

import (
	"testing"
	"time"
)

// The bridge state machine mirrors the Rust twin (mibee-eye-rs
// gb28181_alarm): rising-edge only + cooldown + runtime gate, values
// pinned from the 2022 standard tables.
func TestRisingEdgeOnlyIsAccepted(t *testing.T) {
	b := New(true, 30*time.Second)
	if !b.takeEdge(1_000, true) {
		t.Fatal("first appearance must alarm")
	}
	if b.takeEdge(2_000, true) {
		t.Fatal("sustained presence must not re-alarm")
	}
	if b.takeEdge(3_000, false) {
		t.Fatal("falling edge never alarms")
	}
	if b.takeEdge(4_000, true) {
		t.Fatal("re-appearance within cooldown must be suppressed")
	}
	b.takeEdge(35_000, false)
	if !b.takeEdge(36_000, true) {
		t.Fatal("re-appearance after cooldown must alarm")
	}
}

func TestCooldownSuppressesRapidReRise(t *testing.T) {
	b := New(true, 30*time.Second)
	if !b.takeEdge(10_000, true) {
		t.Fatal("first edge")
	}
	if b.takeEdge(11_000, false) {
		t.Fatal("falling never alarms")
	}
	if b.takeEdge(12_000, true) {
		t.Fatal("re-rise 1s after the last alarm is inside the window")
	}
	if b.takeEdge(30_000, false) {
		t.Fatal("falling never alarms")
	}
	if b.takeEdge(39_999, true) {
		t.Fatal("still inside at +29.999s")
	}
	if b.takeEdge(40_000, false) {
		t.Fatal("falling never alarms")
	}
	if !b.takeEdge(40_000, true) {
		t.Fatal("outside at +30s")
	}
}

func TestMotionGateBlocksAndReenables(t *testing.T) {
	b := New(false, 30*time.Second)
	if b.takeEdge(1_000, true) {
		t.Fatal("gate off at boot suppresses")
	}
	b.SetMotionReporting(true)
	if b.takeEdge(2_000, true) {
		t.Fatal("prev already true — no rising edge")
	}
	b.takeEdge(3_000, false)
	if !b.takeEdge(4_000, true) {
		t.Fatal("gate on: edge accepted")
	}
	b.SetMotionReporting(false)
	b.takeEdge(5_000, false)
	if b.takeEdge(6_000, true) {
		t.Fatal("gate off again suppresses")
	}
}

func TestFirstAlarmHasNoCooldownFloor(t *testing.T) {
	b := New(true, 30*time.Second)
	if !b.takeEdge(0, true) {
		t.Fatal("lastSent starts unset — the very first edge must pass")
	}
}

func TestOnDetectionsWithoutSenderIsSafeSkip(t *testing.T) {
	b := New(true, 30*time.Second)
	if b.OnDetections(1_000, 3) {
		t.Fatal("no sender: safe skip, not a panic or a send")
	}
	if b.OnDetections(2_000, 0) {
		t.Fatal("zero targets never alarms")
	}
}

func TestAlarmValuesAreStandardPinned(t *testing.T) {
	alarm := buildAlarm(1_000, 2)
	if alarm.priority != "4" || alarm.method != "5" || alarm.alarmType != "2" {
		t.Fatalf("field values = %+v", alarm)
	}
	if alarm.time == "" || len(alarm.time) != 19 {
		t.Fatalf("GB time format: %q", alarm.time)
	}
	if alarm.description != "AI moving-target detection: 2 target(s)" {
		t.Fatalf("description: %q", alarm.description)
	}
}

// The send path hands the standard-pinned values to the notifier.
func TestOnDetectionsSendsThroughSender(t *testing.T) {
	b := New(true, 30*time.Second)
	rec := &recordingSender{}
	b.SetSender(rec)
	if !b.OnDetections(5_000, 1) {
		t.Fatal("rising edge with a sender must send")
	}
	if len(rec.calls) != 1 {
		t.Fatalf("sender calls = %d", len(rec.calls))
	}
	got := rec.calls[0]
	if got.priority != "4" || got.method != "5" || got.alarmType != "2" {
		t.Fatalf("sent values = %+v", got)
	}
	// Sustained presence: no second NOTIFY.
	if b.OnDetections(6_000, 2) {
		t.Fatal("sustained presence must not re-send")
	}
}

type recordingSender struct {
	calls []alarmFields
}

func (r *recordingSender) SendAlarm(priority, method, alarmTime, alarmType, description string) bool {
	r.calls = append(r.calls, alarmFields{priority, method, alarmType, alarmTime, description})
	return true
}

func TestMirrorModeToFlips(t *testing.T) {
	cases := []struct {
		mode uint32
		h, v bool
	}{
		{0, false, false},
		{1, true, false},
		{2, false, true},
		{3, true, true},
		{9, false, false},
	}
	for _, c := range cases {
		h, v := MirrorModeToFlips(c.mode)
		if h != c.h || v != c.v {
			t.Fatalf("mode %d = (%v,%v), want (%v,%v)", c.mode, h, v, c.h, c.v)
		}
	}
}

// The SPEC v1 §6 `alarm` SSE sink fires on every accepted rising edge —
// including when no notifier is attached (platform delivery separate).
func TestAcceptedEdgeFiresOnvifSink(t *testing.T) {
	b := New(true, 30*time.Second)
	type edge struct {
		ms      int64
		targets int
	}
	var got []edge
	b.SetOnvifSink(func(nowMs int64, targets int) { got = append(got, edge{nowMs, targets}) })
	if b.OnDetections(5_000, 4) {
		t.Fatal("no sender attached — NOTIFY must not go out")
	}
	if len(got) != 1 || got[0] != (edge{5_000, 4}) {
		t.Fatalf("onvif sink fired %v, want [{5000 4}]", got)
	}
	// Cooldown-suppressed and falling edges must not re-fire.
	if b.OnDetections(6_000, 5) || b.OnDetections(7_000, 0) || len(got) != 1 {
		t.Fatalf("cooldown/falling edge must not re-fire sink: %v", got)
	}
	// Detaching the sink is supported (config off / server stopped).
	b.SetOnvifSink(nil)
	if b.OnDetections(50_000, 1) || len(got) != 1 {
		t.Fatalf("nil sink must be a safe no-op: %v", got)
	}
}

func TestMotionGateBlocksOnvifSink(t *testing.T) {
	b := New(false, 30*time.Second)
	fired := 0
	b.SetOnvifSink(func(int64, int) { fired++ })
	if b.OnDetections(1_000, 1) || fired != 0 {
		t.Fatal("gate off at boot must block the onvif sink too")
	}
}

func TestAcceptedEdgeFiresEventSink(t *testing.T) {
	b := New(true, 30*time.Second)
	var got []int
	b.SetEventSink(func(nowMs int64, targets int) { got = append(got, targets) })
	if b.OnDetections(1_000, 2) {
		t.Fatal("no sender attached — NOTIFY must not go out")
	}
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("sink fired %v, want [2]", got)
	}
	// Cooldown-suppressed edge must not re-fire.
	if b.OnDetections(2_000, 3) || len(got) != 1 {
		t.Fatalf("cooldown must suppress both NOTIFY and sink: %v", got)
	}
}
