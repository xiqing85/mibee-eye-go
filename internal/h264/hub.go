package h264

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// AccessUnit represents a complete H.264 access unit (one or more NALUs).
type AccessUnit struct {
	NALUs     []NALU
	Timestamp time.Time
	KeyFrame  bool // True if contains IDR
}

// Subscriber receives access units from the hub.
type Subscriber struct {
	ID      string
	Channel chan AccessUnit
	cancel  context.CancelFunc

	// dropped counts units lost because this subscriber's channel was full.
	// Consumers that serialize the stream (MSE fMP4) must watch it: a lost
	// unit leaves a reference-frame hole and only an IDR realigns decoders.
	dropped atomic.Uint64
}

// Dropped reports how many access units were dropped for this subscriber
// because its channel buffer was full.
func (s *Subscriber) Dropped() uint64 {
	return s.dropped.Load()
}

// AUHub fans out access units to multiple subscribers.
// Thread-safe via embedded mutex.
type AUHub struct {
	mu                   sync.Mutex
	subscribers          map[string]*Subscriber
	nextID               int
	droppedAUs           atomic.Uint64 // total dropped AUs across all subscribers
	subscriberBufferSize int
}

func NewAUHub() *AUHub {
	return NewAUHubWithSize(64)
}

// NewAUHubWithSize creates a new access-unit fan-out hub with the given subscriber buffer size.
func NewAUHubWithSize(size int) *AUHub {
	return &AUHub{
		subscribers:          make(map[string]*Subscriber),
		subscriberBufferSize: size,
	}
}

// StartDropLogger periodically logs dropped AU statistics.
// Call once at startup; cancel via ctx to stop.
func (h *AUHub) StartDropLogger(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		var last uint64
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cur := h.droppedAUs.Load()
				if cur > last {
					slog.Warn("h264: AUs dropped in last 30s (slow subscribers)", "dropped", cur-last)
					last = cur
				}
			}
		}
	}()
}

// Write adds an access unit to the hub for distribution.
// Non-blocking: drops AU to a subscriber if its channel buffer is full.
func (h *AUHub) Write(au AccessUnit) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, sub := range h.subscribers {
		select {
		case sub.Channel <- au:
		default:
			// Subscriber too slow — drop frame to avoid blocking the writer.
			// Per-subscriber count lets the consumer detect the hole.
			sub.dropped.Add(1)
			h.droppedAUs.Add(1)
		}
	}
}

// DroppedAUs returns the total number of access units dropped due to slow subscribers.
func (h *AUHub) DroppedAUs() uint64 {
	return h.droppedAUs.Load()
}

// Subscribe registers a new subscriber and returns it.
// The subscriber is automatically removed when ctx is cancelled.
func (h *AUHub) Subscribe(ctx context.Context) *Subscriber {
	h.mu.Lock()
	h.nextID++
	id := string(rune(h.nextID)) // simple unique ID
	ctx, cancel := context.WithCancel(ctx)

	sub := &Subscriber{
		ID:      id,
		Channel: make(chan AccessUnit, h.subscriberBufferSize),
		cancel:  cancel,
	}
	h.subscribers[id] = sub
	h.mu.Unlock()

	go func() {
		defer h.Unsubscribe(id)
		<-ctx.Done()
	}()

	return sub
}

func (h *AUHub) Unsubscribe(id string) {
	h.mu.Lock()
	sub, ok := h.subscribers[id]
	if !ok {
		h.mu.Unlock()
		return
	}
	delete(h.subscribers, id)
	close(sub.Channel)
	h.mu.Unlock()
	sub.cancel()
}

// SubscriberCount returns the current number of active subscribers.
func (h *AUHub) SubscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subscribers)
}
