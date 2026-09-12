package recording

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/config"
)

// Retention enforces the recording retention policy: it periodically
// deletes segments older than RetentionDays and prunes the oldest
// segments until total storage is under MaxStorageMB. It updates the
// index after each deletion.
type Retention struct {
	cfg   config.RecordingConfig
	index *Index
	root  string
}

// NewRetention creates a Retention policy over the given index and root.
func NewRetention(cfg config.RecordingConfig, index *Index, root string) *Retention {
	return &Retention{cfg: cfg, index: index, root: root}
}

// Run runs the retention loop until ctx is cancelled, checking every
// interval (default 10 minutes).
func (r *Retention) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Enforce()
		}
	}
}

// Enforce applies the retention policy once: age-based deletion first,
// then capacity-based pruning.
func (r *Retention) Enforce() {
	r.enforceAge()
	r.enforceCapacity()
}

// enforceAge deletes segments older than RetentionDays (0 = infinite).
func (r *Retention) enforceAge() {
	if r.cfg.RetentionDays <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(r.cfg.RetentionDays) * 24 * time.Hour).UnixMilli()
	for _, si := range r.index.Snapshot() {
		if si.EndMS < cutoff {
			r.deleteSegment(si)
		}
	}
}

// enforceCapacity prunes the oldest segments until total size is under
// MaxStorageMB (0 = unlimited).
func (r *Retention) enforceCapacity() {
	if r.cfg.MaxStorageMB <= 0 {
		return
	}
	limit := int64(r.cfg.MaxStorageMB) * 1024 * 1024
	for r.index.TotalSize() > limit {
		segs := r.index.Snapshot()
		if len(segs) == 0 {
			return
		}
		// Snapshot is sorted by StartMS, so the oldest is first.
		r.deleteSegment(segs[0])
	}
}

// deleteSegment removes a segment file, its sidecar, and its index record.
func (r *Retention) deleteSegment(si SegmentInfo) {
	abs := filepath.Join(r.root, si.File)
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		slog.Warn("recording: retention remove segment failed", "file", si.File, "error", err)
	}
	// Best-effort sidecar removal.
	_ = os.Remove(abs + ".ts.jsonl")
	if err := r.index.Remove(si.File); err != nil {
		slog.Warn("recording: retention index update failed", "file", si.File, "error", err)
	}
	slog.Info("recording: retention removed segment", "file", si.File, "size", si.Size)
}
