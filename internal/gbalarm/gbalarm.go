// Package gbalarm bridges AI detections into alarm fan-out: the GB/T
// 28181 alarm NOTIFY (§9.5 / A.2.5), the SPEC v1 §6 `alarm` SSE event,
// and the ONVIF Pull-Point MotionAlarm event — one accepted rising
// edge, three sinks.
//
// Per the 2022 standard's value tables an analytics alarm is
// AlarmMethod=5 (视频报警) with AlarmType=2 (运动目标检测报警); priority
// is 4 (四级警情 — informational analytics, not a physical sensor).
// NOTIFYs fire on the RISING edge of "targets present" with a cooldown,
// so a busy scene cannot storm the platform, and the platform's
// DeviceConfig(AlarmReport) switch (A.2.3.2.10) gates motion reporting
// at runtime. Everything is a no-op until a platform actually
// SUBSCRIBEs — the library's notifier handles that. State machine twin
// of mibee-eye-rs gb28181_alarm.
package gbalarm

import (
	"fmt"
	"sync/atomic"
	"time"
)

// Sender is the notifier half (satisfied by gb28181-go's
// *device.DeviceNotifier).
type Sender interface {
	SendAlarm(priority, method, alarmTime, alarmType, description string) bool
}

// DefaultCooldown is the anti-storm spacing between alarm NOTIFYs.
const DefaultCooldown = 30 * time.Second

// Bridge feeds AI detection batches into alarm NOTIFYs. Safe for
// concurrent use (AI loop + DeviceConfig handler + server lifecycle).
type Bridge struct {
	sender atomic.Pointer[Sender]
	// Runtime gate from DeviceConfig(AlarmReport) MotionDetection
	// (0 off, 1 on). Boot default comes from config.
	motionReporting atomic.Bool
	prevTarget      atomic.Bool
	// Epoch-ms of the last accepted alarm; negative = never.
	lastSentMs atomic.Int64
	cooldownMs int64
	// onEdge is the optional rising-edge listener (epoch-ms, target
	// count) for the web SSE alarm event.
	onEdge atomic.Pointer[func(nowMs int64, targets int)]
	// onOnvifEdge is the optional rising-edge listener feeding the
	// ONVIF events service (epoch-ms, target count) — same accepted
	// edge again, independent of both the SSE hub and NOTIFY delivery.
	onOnvifEdge atomic.Pointer[func(nowMs int64, targets int)]
}

// New builds the bridge; enabled is the boot default of the
// AlarmReport gate.
func New(enabled bool, cooldown time.Duration) *Bridge {
	b := &Bridge{cooldownMs: cooldown.Milliseconds()}
	b.motionReporting.Store(enabled)
	b.lastSentMs.Store(-1)
	return b
}

// SetEventSink installs the rising-edge listener (SPEC v1 §6 `alarm`
// SSE event) — same accepted-edge source as the NOTIFY, independent of
// platform delivery.
func (b *Bridge) SetEventSink(f func(nowMs int64, targets int)) {
	b.onEdge.Store(&f)
}

// SetOnvifSink installs (or with nil detaches) the ONVIF events
// listener (main.go wraps the onvif-go server's PublishEvent with the
// MotionAlarm shape).
func (b *Bridge) SetOnvifSink(f func(nowMs int64, targets int)) {
	if f == nil {
		b.onOnvifEdge.Store(nil)
		return
	}
	b.onOnvifEdge.Store(&f)
}

// SetSender installs the live notifier (the GB28181 server owns it).
func (b *Bridge) SetSender(s Sender) {
	if s == nil {
		b.sender.Store(nil)
		return
	}
	b.sender.Store(&s)
}

// SetMotionReporting toggles the DeviceConfig(AlarmReport)
// MotionDetection gate (0 off, 1 on).
func (b *Bridge) SetMotionReporting(on bool) {
	b.motionReporting.Store(on)
}

// OnDetections feeds one detection batch (epoch-ms clock for
// testability); returns whether an alarm NOTIFY went out. A
// decided-but-unsent alarm (server down / nobody subscribed) is not
// retried — the next one rides the next rising edge.
func (b *Bridge) OnDetections(nowMs int64, targetCount int) bool {
	if !b.takeEdge(nowMs, targetCount > 0) {
		return false
	}
	// The SPEC v1 §6 `alarm` SSE event rides the accepted edge
	// regardless of whether a NOTIFY can go out.
	if f := b.onEdge.Load(); f != nil {
		(*f)(nowMs, targetCount)
	}
	// So does the ONVIF MotionAlarm (no NVR subscribed = no-op).
	if f := b.onOnvifEdge.Load(); f != nil {
		(*f)(nowMs, targetCount)
	}
	sender := b.sender.Load()
	if sender == nil {
		return false
	}
	a := buildAlarm(nowMs, targetCount)
	return (*sender).SendAlarm(a.priority, a.method, a.time, a.alarmType, a.description)
}

// takeEdge is the rising-edge + gate + cooldown state machine; books
// lastSentMs when it accepts an edge.
func (b *Bridge) takeEdge(nowMs int64, hasTarget bool) bool {
	prev := b.prevTarget.Swap(hasTarget)
	if !hasTarget || prev {
		return false
	}
	if !b.motionReporting.Load() {
		return false
	}
	last := b.lastSentMs.Load()
	if last >= 0 && nowMs-last < b.cooldownMs {
		return false
	}
	b.lastSentMs.Store(nowMs)
	return true
}

// alarmFields carries the standard-pinned NOTIFY values.
type alarmFields struct {
	priority    string
	method      string
	alarmType   string
	time        string
	description string
}

// buildAlarm renders the §9.5 values (method 5 视频报警 → type 2 运动目标
// 检测报警; priority 4 四级警情) with the GB time format.
func buildAlarm(nowMs int64, targetCount int) alarmFields {
	t := time.UnixMilli(nowMs).Format("2006-01-02T15:04:05")
	return alarmFields{
		priority:    "4",
		method:      "5",
		alarmType:   "2",
		time:        t,
		description: fmt.Sprintf("AI moving-target detection: %d target(s)", targetCount),
	}
}

// MirrorModeToFlips maps the A.2.1.22 frameMirrorCfgType mode onto
// device flips: 0 不启用, 1 水平镜像 (hflip), 2 上下镜像 (vflip),
// 3 中心镜像 (both). Anything else leaves the frames untouched
// (defensive — the library only decodes 0-3).
func MirrorModeToFlips(mode uint32) (hFlip, vFlip bool) {
	switch mode {
	case 1:
		return true, false
	case 2:
		return false, true
	case 3:
		return true, true
	default:
		return false, false
	}
}
