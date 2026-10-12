package onvifgo

// Interop pin: the MotionAlarm event this camera publishes must parse
// through the onvif-go client's standard motion helper (events.ParseMotionAlarm,
// library v2.4.0) — that is what NVR consumers run on their side. A drift in
// the topic string or the State item silently breaks every parser that
// recognizes only the standard shapes.

import (
	"testing"

	onvifevents "github.com/mickeyzzc/onvif-go/v2/events"
	onvifserver "github.com/mickeyzzc/onvif-go/v2/server"
	onviftypes "github.com/mickeyzzc/onvif-go/v2/types"
)

func TestMotionAlarmParsesWithClientHelper(t *testing.T) {
	evt := motionAlarmEvent(3)

	// The NotificationMessage a subscriber decodes from the wire carries
	// the same Topic/SimpleItems the server event publishes.
	msg := onvifevents.NotificationMessage{
		Topic: evt.Topic,
		Message: onvifevents.EventMessage{
			Source: serverItemsToClient(evt.Source),
			Data:   serverItemsToClient(evt.Data),
		},
	}

	alarm, ok := onvifevents.ParseMotionAlarm(msg)
	if !ok {
		t.Fatalf("published MotionAlarm not recognized by events.ParseMotionAlarm: topic=%q data=%+v", evt.Topic, evt.Data)
	}

	if !alarm.Active {
		t.Error("State=true must decode as Active")
	}

	if alarm.Source != "0" {
		t.Errorf("Source = %q, want the SPEC single-camera id \"0\"", alarm.Source)
	}
}

// serverItemsToClient mirrors the wire SimpleItem mapping.
func serverItemsToClient(items []onvifserver.SimpleItem) []onviftypes.SimpleItem {
	out := make([]onviftypes.SimpleItem, len(items))
	for i, it := range items {
		out[i] = onviftypes.SimpleItem{Name: it.Name, Value: it.Value}
	}

	return out
}
