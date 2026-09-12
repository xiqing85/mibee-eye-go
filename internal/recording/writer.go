package recording

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/config"
	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/h264"
)

// startCode is the 4-byte Annex-B start code prefix written before each NALU.
var startCode = []byte{0x00, 0x00, 0x00, 0x01}

// droppedLogInterval is how often the Writer logs a slow-consumer warning.
const droppedLogInterval = 10 * time.Second

// queueCapacity is the Writer's dedicated AU buffer (~600 AUs ≈ 20s @30fps).
const queueCapacity = 600

// Writer subscribes to the AUHub and accumulates access units into
// segment files. Segments roll over every SegmentSecs OR at the first
// keyframe at/after that boundary, so every segment starts with an IDR.
//
// The Writer must never block the AUHub broadcast path. A drain goroutine
// pulls AUs from the hub's subscriber channel into a dedicated ring buffer
// (queueCapacity ≈ 20s @30fps). If the consumer falls more than one buffer
// behind, the drain drops the OLDEST non-keyframe AU (keeping the stream
// decodable — the next IDR re-syncs) and increments a dropped counter,
// logging a warning every 10s. Dropping never blocks the live path.
type Writer struct {
	cfg   config.RecordingConfig
	hub   *h264.AUHub
	index *Index
	root  string

	mu      sync.Mutex
	queue   []h264.AccessUnit // ring buffer of pending AUs
	notify  chan struct{}     // signals the segment loop that queue is non-empty
	dropped uint64            // AUs dropped because the consumer fell behind

	// segment state (guarded by mu)
	segFile      *os.File
	segSidecar   *os.File
	segPath      string // absolute path of current .h264
	segRel       string // path relative to root
	segStart     time.Time
	segStartMS   int64 // unix ms of first frame
	segLastMS    int64 // unix ms of last written frame
	segFrames    int
	segKeyframes int
	segBytes     int64
	segRollAt    time.Time // earliest time at which a keyframe triggers roll
	active       bool      // reflects recording state for DeviceStatus <Record>
}

// NewWriter creates a Writer for the given hub and recording config.
func NewWriter(hub *h264.AUHub, cfg config.RecordingConfig) *Writer {
	return &Writer{
		cfg:    cfg,
		hub:    hub,
		index:  NewIndex(filepath.Join(cfg.StoragePath, "index.jsonl")),
		root:   cfg.StoragePath,
		notify: make(chan struct{}, 1),
	}
}

// Index exposes the writer's index (for retention and queries).
func (w *Writer) Index() *Index { return w.index }

// Active reports whether the writer is currently recording.
func (w *Writer) Active() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.active
}

// Run starts the recording loop. It blocks until ctx is cancelled.
func (w *Writer) Run(ctx context.Context) error {
	if err := os.MkdirAll(w.root, 0o755); err != nil {
		return fmt.Errorf("creating recording root %s: %w", w.root, err)
	}
	if err := w.index.Load(w.index.path); err != nil {
		return fmt.Errorf("loading recording index: %w", err)
	}

	sub := w.hub.Subscribe(ctx)
	defer w.hub.Unsubscribe(sub.ID)

	// Drain goroutine: move AUs from the hub channel into the ring buffer,
	// dropping the oldest non-keyframe when full. Never blocks the hub.
	go w.drain(ctx, sub.Channel)

	lastDropLog := time.Now()
	for {
		select {
		case <-ctx.Done():
			w.closeSegment()
			return nil
		case <-w.notify:
			for {
				au, ok := w.pop()
				if !ok {
					break
				}
				w.handleAU(au)
			}
			if w.dropped > 0 && time.Since(lastDropLog) >= droppedLogInterval {
				slog.Warn("recording: dropped AUs (consumer behind)", "dropped", w.dropped)
				w.dropped = 0
				lastDropLog = time.Now()
			}
		}
	}
}

// drain pulls AUs from the hub channel into the ring buffer.
func (w *Writer) drain(ctx context.Context, ch <-chan h264.AccessUnit) {
	for {
		select {
		case <-ctx.Done():
			return
		case au, ok := <-ch:
			if !ok {
				return
			}
			w.push(au)
		}
	}
}

// push appends an AU to the ring buffer, dropping the oldest non-keyframe
// when the buffer is full. Caller need not hold w.mu.
func (w *Writer) push(au h264.AccessUnit) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queue) >= queueCapacity {
		// Drop the oldest non-keyframe AU to make room.
		for i, q := range w.queue {
			if !q.KeyFrame {
				w.queue = append(w.queue[:i], w.queue[i+1:]...)
				w.dropped++
				break
			}
		}
		// If every buffered AU is a keyframe, drop the oldest anyway.
		if len(w.queue) >= queueCapacity {
			w.queue = w.queue[1:]
			w.dropped++
		}
	}
	w.queue = append(w.queue, au)
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// pop removes and returns the oldest AU, or ok=false if the queue is empty.
func (w *Writer) pop() (h264.AccessUnit, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queue) == 0 {
		return h264.AccessUnit{}, false
	}
	au := w.queue[0]
	w.queue = w.queue[1:]
	return au, true
}

// handleAU processes a single access unit.
func (w *Writer) handleAU(au h264.AccessUnit) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// If we have no open segment, start one (first AU or after a roll).
	if w.segFile == nil {
		if err := w.openSegmentLocked(au); err != nil {
			slog.Error("recording: open segment failed", "error", err)
			return
		}
	}

	// Roll at a keyframe at/after the target boundary.
	if au.KeyFrame && !w.segRollAt.IsZero() && !au.Timestamp.Before(w.segRollAt) {
		if err := w.closeSegmentLocked(); err != nil {
			slog.Error("recording: close segment failed", "error", err)
		}
		if err := w.openSegmentLocked(au); err != nil {
			slog.Error("recording: open segment failed", "error", err)
			return
		}
	}

	// Write the AU's NALUs as Annex-B.
	for _, nalu := range au.NALUs {
		if _, err := w.segFile.Write(startCode); err != nil {
			slog.Error("recording: write start code failed", "error", err)
			return
		}
		if _, err := w.segFile.Write(nalu.Data); err != nil {
			slog.Error("recording: write NALU failed", "error", err)
			return
		}
		w.segBytes += int64(len(startCode) + len(nalu.Data))
	}

	// Sidecar: one {"pts_ms":N} line per frame, relative to segment start.
	pts := au.Timestamp.Sub(w.segStart).Milliseconds()
	line, _ := json.Marshal(struct {
		PTSMS int64 `json:"pts_ms"`
	}{PTSMS: pts})
	if _, err := w.segSidecar.Write(append(line, '\n')); err != nil {
		slog.Error("recording: write sidecar failed", "error", err)
	}

	w.segFrames++
	w.segLastMS = au.Timestamp.UnixMilli()
	if au.KeyFrame {
		w.segKeyframes++
	}
}

// openSegmentLocked opens a new segment file and its sidecar.
// Caller must hold w.mu.
func (w *Writer) openSegmentLocked(au h264.AccessUnit) error {
	now := au.Timestamp
	dir := filepath.Join(w.root, now.Format("2006-01-02"), now.Format("15"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating segment dir %s: %w", dir, err)
	}
	name := now.Format("150405") + ".h264"
	rel := filepath.Join(now.Format("2006-01-02"), now.Format("15"), name)
	abs := filepath.Join(w.root, rel)

	f, err := os.OpenFile(abs, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening segment %s: %w", abs, err)
	}
	side, err := os.OpenFile(abs+".ts.jsonl", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		f.Close()
		return fmt.Errorf("opening sidecar %s: %w", abs+".ts.jsonl", err)
	}

	w.segFile = f
	w.segSidecar = side
	w.segPath = abs
	w.segRel = rel
	w.segStart = now
	w.segStartMS = now.UnixMilli()
	w.segLastMS = now.UnixMilli()
	w.segFrames = 0
	w.segKeyframes = 0
	w.segBytes = 0
	w.segRollAt = now.Add(time.Duration(w.cfg.SegmentSecs) * time.Second)
	w.active = true
	return nil
}

// closeSegmentLocked finalizes the current segment: syncs, closes,
// appends the index record, and clears segment state.
// Caller must hold w.mu.
func (w *Writer) closeSegmentLocked() error {
	if w.segFile == nil {
		return nil
	}
	// f.Sync() on segment close only — SD-card friendly (no per-AU sync).
	if err := w.segFile.Sync(); err != nil {
		slog.Warn("recording: segment sync failed", "error", err)
	}
	if err := w.segFile.Close(); err != nil {
		return fmt.Errorf("closing segment %s: %w", w.segPath, err)
	}
	if err := w.segSidecar.Close(); err != nil {
		return fmt.Errorf("closing sidecar %s: %w", w.segPath+".ts.jsonl", err)
	}

	si := SegmentInfo{
		File:      w.segRel,
		StartMS:   w.segStartMS,
		EndMS:     w.segLastMS,
		Size:      w.segBytes,
		Frames:    w.segFrames,
		Keyframes: w.segKeyframes,
	}
	if err := w.index.Append(si); err != nil {
		slog.Error("recording: index append failed", "error", err)
	}

	w.segFile = nil
	w.segSidecar = nil
	w.segPath = ""
	w.segRel = ""
	w.active = false
	return nil
}

// closeSegment is the unlocked wrapper used on shutdown.
func (w *Writer) closeSegment() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.closeSegmentLocked(); err != nil {
		slog.Error("recording: close segment on shutdown failed", "error", err)
	}
}
