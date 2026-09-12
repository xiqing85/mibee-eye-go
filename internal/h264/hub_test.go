package h264

import (
	"context"
	"sync"
	"testing"
	"time"
)

func makeTestAU(keyFrame bool) AccessUnit {
	typ := byte(0x41) // non-IDR
	if keyFrame {
		typ = 0x65 // IDR
	}
	return AccessUnit{
		NALUs:     []NALU{{Type: typ & 0x1F, Data: []byte{typ, 0x00, 0x01}}},
		Timestamp: time.Now(),
		KeyFrame:  keyFrame,
	}
}

func collectAUs(ch <-chan AccessUnit, count int, timeout time.Duration) []AccessUnit {
	var result []AccessUnit
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for len(result) < count {
		select {
		case au, ok := <-ch:
			if !ok {
				return result
			}
			result = append(result, au)
		case <-timer.C:
			return result
		}
	}
	return result
}

func TestSingleSubscriber(t *testing.T) {
	hub := NewAUHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub := hub.Subscribe(ctx)
	if hub.SubscriberCount() != 1 {
		t.Fatalf("expected 1 subscriber, got %d", hub.SubscriberCount())
	}

	const n = 10
	for i := 0; i < n; i++ {
		hub.Write(makeTestAU(i%3 == 0))
	}

	received := collectAUs(sub.Channel, n, time.Second)
	if len(received) != n {
		t.Fatalf("expected %d AUs, got %d", n, len(received))
	}
	for i, au := range received {
		wantKey := i%3 == 0
		if au.KeyFrame != wantKey {
			t.Errorf("AU[%d]: KeyFrame=%v, want %v", i, au.KeyFrame, wantKey)
		}
	}
}

func TestMultipleSubscribers(t *testing.T) {
	hub := NewAUHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const numSubs = 3
	subs := make([]*Subscriber, numSubs)
	for i := range subs {
		subs[i] = hub.Subscribe(ctx)
	}
	if hub.SubscriberCount() != numSubs {
		t.Fatalf("expected %d subscribers, got %d", numSubs, hub.SubscriberCount())
	}

	const n = 10
	for i := 0; i < n; i++ {
		hub.Write(makeTestAU(i%2 == 0))
	}

	var wg sync.WaitGroup
	results := make([][]AccessUnit, numSubs)

	for i, sub := range subs {
		wg.Add(1)
		go func(idx int, ch <-chan AccessUnit) {
			defer wg.Done()
			results[idx] = collectAUs(ch, n, time.Second)
		}(i, sub.Channel)
	}
	wg.Wait()

	for i, res := range results {
		if len(res) != n {
			t.Errorf("subscriber %d: expected %d AUs, got %d", i, n, len(res))
		}
	}
}

func TestSubscribeUnsubscribe(t *testing.T) {
	hub := NewAUHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub := hub.Subscribe(ctx)

	// Write 3 AUs before unsubscribe.
	for i := 0; i < 3; i++ {
		hub.Write(makeTestAU(false))
	}
	before := collectAUs(sub.Channel, 3, time.Second)
	if len(before) != 3 {
		t.Fatalf("before unsubscribe: expected 3 AUs, got %d", len(before))
	}

	// Unsubscribe.
	hub.Unsubscribe(sub.ID)
	if hub.SubscriberCount() != 0 {
		t.Errorf("expected 0 subscribers after unsubscribe, got %d", hub.SubscriberCount())
	}

	// Channel should be closed.
	_, ok := <-sub.Channel
	if ok {
		t.Error("expected channel to be closed after unsubscribe")
	}

	// Write more AUs — should not panic.
	hub.Write(makeTestAU(true))
	hub.Write(makeTestAU(false))
}

func TestContextCancellation(t *testing.T) {
	hub := NewAUHub()
	ctx, cancel := context.WithCancel(context.Background())

	sub := hub.Subscribe(ctx)

	// Write a couple AUs.
	hub.Write(makeTestAU(true))
	_ = collectAUs(sub.Channel, 1, time.Second)

	// Cancel context — should trigger unsubscribe.
	cancel()

	// Wait briefly for goroutine to clean up.
	time.Sleep(50 * time.Millisecond)

	if hub.SubscriberCount() != 0 {
		t.Errorf("expected 0 subscribers after context cancel, got %d", hub.SubscriberCount())
	}

	// Channel should be closed.
	_, ok := <-sub.Channel
	if ok {
		t.Error("expected channel to be closed after context cancellation")
	}
}

func TestZeroSubscribers(t *testing.T) {
	hub := NewAUHub()

	if hub.SubscriberCount() != 0 {
		t.Fatalf("expected 0 subscribers, got %d", hub.SubscriberCount())
	}

	// Writing with no subscribers must not block or panic.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			hub.Write(makeTestAU(true))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Write blocked with 0 subscribers")
	}
}

func TestDoubleUnsubscribe(t *testing.T) {
	hub := NewAUHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub := hub.Subscribe(ctx)
	hub.Unsubscribe(sub.ID)
	// Second unsubscribe should be safe (no-op, no panic).
	hub.Unsubscribe(sub.ID)
}

func TestUnsubscribeUnknownID(t *testing.T) {
	hub := NewAUHub()
	// Unsubscribe an ID that was never subscribed — no panic.
	hub.Unsubscribe("nonexistent")
}

func TestSubscriberCleanup(t *testing.T) {
	hub := NewAUHub()
	const n = 100
	cancels := make([]context.CancelFunc, n)

	for i := 0; i < n; i++ {
		var ctx context.Context
		ctx, cancels[i] = context.WithCancel(context.Background())
		hub.Subscribe(ctx)
	}

	if count := hub.SubscriberCount(); count != n {
		t.Fatalf("expected %d subscribers, got %d", n, count)
	}

	// Cancel all contexts.
	for i := 0; i < n; i++ {
		cancels[i]()
	}

	// Wait for goroutines to clean up.
	time.Sleep(50 * time.Millisecond)

	if count := hub.SubscriberCount(); count != 0 {
		t.Errorf("expected 0 subscribers after cancel, got %d", count)
	}
}

func TestSubscribeLeak100Cycles(t *testing.T) {
	hub := NewAUHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 0; i < 100; i++ {
		sub := hub.Subscribe(ctx)
		if count := hub.SubscriberCount(); count != 1 {
			t.Fatalf("cycle %d: expected 1 subscriber, got %d", i, count)
		}
		hub.Unsubscribe(sub.ID)
		if count := hub.SubscriberCount(); count != 0 {
			t.Fatalf("cycle %d: expected 0 subscribers after unsubscribe, got %d", i, count)
		}
	}
}

// TestSubscriberDroppedCounter verifies the per-subscriber drop counter that
// stream serializers (MSE fMP4) watch to realign on the next IDR.
func TestSubscriberDroppedCounter(t *testing.T) {
	hub := NewAUHubWithSize(2)
	sub := hub.Subscribe(t.Context())
	defer hub.Unsubscribe(sub.ID)

	for range 5 {
		hub.Write(AccessUnit{KeyFrame: true, NALUs: []NALU{{Data: []byte{0x67}}}})
	}

	if got := sub.Dropped(); got != 3 {
		t.Fatalf("Dropped() = %d, want 3 (5 written, buffer 2, none read)", got)
	}
	if got := hub.DroppedAUs(); got != 3 {
		t.Fatalf("DroppedAUs() = %d, want 3", got)
	}
	select {
	case <-sub.Channel:
	default:
		t.Fatal("buffered units must still be delivered")
	}
	if got := sub.Dropped(); got != 3 {
		t.Fatalf("Dropped() changed after draining: %d", got)
	}
}
