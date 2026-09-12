package recording

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/config"
	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/h264"
)

func testCfg(root string) config.RecordingConfig {
	return config.RecordingConfig{
		Enabled:       true,
		StoragePath:   root,
		SegmentSecs:   60,
		RetentionDays: 0,
		MaxStorageMB:  0,
	}
}

// waitForSubscriber blocks until the hub has exactly one subscriber
// (the writer), so tests don't write AUs before the writer subscribes.
func waitForSubscriber(t *testing.T, hub *h264.AUHub) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.SubscriberCount() == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("writer did not subscribe within timeout")
}

// --- Index tests ---

func TestIndex_WriteLookup_Hit(t *testing.T) {
	dir := t.TempDir()
	ix := NewIndex(filepath.Join(dir, "index.jsonl"))
	if err := ix.Load(ix.path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	si := SegmentInfo{File: "2026-08-15/10/0000.h264", StartMS: 1000, EndMS: 2000, Size: 100, Frames: 30, Keyframes: 1}
	if err := ix.Append(si); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got := ix.Lookup(1500, 1800)
	if len(got) != 1 || got[0].File != si.File {
		t.Fatalf("Lookup hit: got %+v", got)
	}
}

func TestIndex_WriteLookup_Overlap(t *testing.T) {
	dir := t.TempDir()
	ix := NewIndex(filepath.Join(dir, "index.jsonl"))
	if err := ix.Load(ix.path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	segs := []SegmentInfo{
		{File: "a.h264", StartMS: 0, EndMS: 100},
		{File: "b.h264", StartMS: 200, EndMS: 300},
		{File: "c.h264", StartMS: 400, EndMS: 500},
	}
	for _, s := range segs {
		if err := ix.Append(s); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	// Range overlapping b only.
	got := ix.Lookup(250, 250)
	if len(got) != 1 || got[0].File != "b.h264" {
		t.Fatalf("Lookup overlap: got %+v", got)
	}
	// Range spanning a and b.
	got = ix.Lookup(50, 250)
	if len(got) != 2 {
		t.Fatalf("Lookup span: got %d segments", len(got))
	}
}

func TestIndex_WriteLookup_Empty(t *testing.T) {
	dir := t.TempDir()
	ix := NewIndex(filepath.Join(dir, "index.jsonl"))
	if err := ix.Load(ix.path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := ix.Lookup(0, 100); len(got) != 0 {
		t.Fatalf("expected empty lookup, got %+v", got)
	}
	// Empty range (start > end) returns nil.
	if got := ix.Lookup(100, 0); got != nil {
		t.Fatalf("expected nil for empty range, got %+v", got)
	}
}

func TestIndex_Load_MissingFile(t *testing.T) {
	dir := t.TempDir()
	ix := NewIndex(filepath.Join(dir, "nope.jsonl"))
	if err := ix.Load(ix.path); err != nil {
		t.Fatalf("Load missing file should be tolerated: %v", err)
	}
	if len(ix.Snapshot()) != 0 {
		t.Fatalf("expected empty index")
	}
}

func TestIndex_Load_SkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.jsonl")
	content := "not-json\n{\"file\":\"ok.h264\",\"start_ms\":1,\"end_ms\":2,\"size\":3,\"frames\":4,\"keyframes\":1}\n{\"broken\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ix := NewIndex(path)
	if err := ix.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := ix.Snapshot()
	if len(got) != 1 || got[0].File != "ok.h264" {
		t.Fatalf("expected 1 valid segment, got %+v", got)
	}
}

func TestIndex_Remove_RewritesFile(t *testing.T) {
	dir := t.TempDir()
	ix := NewIndex(filepath.Join(dir, "index.jsonl"))
	if err := ix.Load(ix.path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	segs := []SegmentInfo{
		{File: "a.h264", StartMS: 0, EndMS: 100},
		{File: "b.h264", StartMS: 200, EndMS: 300},
	}
	for _, s := range segs {
		if err := ix.Append(s); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := ix.Remove("a.h264"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got := ix.Snapshot()
	if len(got) != 1 || got[0].File != "b.h264" {
		t.Fatalf("expected only b.h264, got %+v", got)
	}
	// Reload from disk to confirm the rewrite persisted.
	ix2 := NewIndex(ix.path)
	if err := ix2.Load(ix.path); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if len(ix2.Snapshot()) != 1 || ix2.Snapshot()[0].File != "b.h264" {
		t.Fatalf("reloaded index should have only b.h264, got %+v", ix2.Snapshot())
	}
}

// --- Retention tests ---

func TestRetention_DeletesOldSegmentsAndSyncsIndex(t *testing.T) {
	dir := t.TempDir()
	cfg := testCfg(dir)
	cfg.RetentionDays = 1
	ix := NewIndex(filepath.Join(dir, "index.jsonl"))
	if err := ix.Load(ix.path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-time.Hour)
	segs := []SegmentInfo{
		{File: "old.h264", StartMS: old.UnixMilli(), EndMS: old.Add(time.Second).UnixMilli(), Size: 10},
		{File: "recent.h264", StartMS: recent.UnixMilli(), EndMS: recent.Add(time.Second).UnixMilli(), Size: 10},
	}
	for _, s := range segs {
		if err := ix.Append(s); err != nil {
			t.Fatalf("Append: %v", err)
		}
		// Create the actual files so retention can remove them.
		if err := os.WriteFile(filepath.Join(dir, s.File), []byte("x"), 0o644); err != nil {
			t.Fatalf("write seg: %v", err)
		}
	}
	ret := NewRetention(cfg, ix, dir)
	ret.Enforce()
	got := ix.Snapshot()
	if len(got) != 1 || got[0].File != "recent.h264" {
		t.Fatalf("expected only recent.h264 after age retention, got %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.h264")); !os.IsNotExist(err) {
		t.Fatalf("old.h264 should be deleted, stat err=%v", err)
	}
}

func TestRetention_CapacityPrunesOldestFirst(t *testing.T) {
	dir := t.TempDir()
	cfg := testCfg(dir)
	cfg.MaxStorageMB = 0 // 0 = unlimited; use a tiny cap via MB is awkward, so test with 1MB and small files
	cfg.MaxStorageMB = 1
	ix := NewIndex(filepath.Join(dir, "index.jsonl"))
	if err := ix.Load(ix.path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 3 segments of 1MB each = 3MB total > 1MB cap. Pruning oldest-first
	// removes a then b (2MB > 1MB), leaving only c (1MB, not > 1MB).
	segs := []SegmentInfo{
		{File: "a.h264", StartMS: 0, EndMS: 100, Size: 1024 * 1024},
		{File: "b.h264", StartMS: 200, EndMS: 300, Size: 1024 * 1024},
		{File: "c.h264", StartMS: 400, EndMS: 500, Size: 1024 * 1024},
	}
	for _, s := range segs {
		if err := ix.Append(s); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, s.File), make([]byte, s.Size), 0o644); err != nil {
			t.Fatalf("write seg: %v", err)
		}
	}
	ret := NewRetention(cfg, ix, dir)
	ret.Enforce()
	// After pruning oldest-first until under 1MB, only c.h264 (the newest) remains.
	got := ix.Snapshot()
	if len(got) != 1 || got[0].File != "c.h264" {
		t.Fatalf("expected only c.h264 after capacity pruning, got %+v", got)
	}
}

// --- Writer tests ---

// syntheticAU builds an AccessUnit with the given NALU types and timestamp.
func syntheticAU(ts time.Time, key bool, types ...byte) h264.AccessUnit {
	nalus := make([]h264.NALU, 0, len(types))
	for _, ty := range types {
		nalus = append(nalus, h264.NALU{Type: ty, Data: []byte{ty, 0x01, 0x02, 0x03}, IsIDR: ty == 5, IsSPS: ty == 7, IsPPS: ty == 8})
	}
	return h264.AccessUnit{NALUs: nalus, Timestamp: ts, KeyFrame: key}
}

func TestWriter_SegmentRollAtKeyframe(t *testing.T) {
	dir := t.TempDir()
	cfg := testCfg(dir)
	cfg.SegmentSecs = 60
	hub := h264.NewAUHubWithSize(64)
	w := NewWriter(hub, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	waitForSubscriber(t, hub)

	base := time.Now()
	// Feed frames every 100ms. First frame is a keyframe (starts segment 1).
	hub.Write(syntheticAU(base, true, 7, 8, 5))
	for i := 1; i < 20; i++ {
		hub.Write(syntheticAU(base.Add(time.Duration(i)*100*time.Millisecond), false, 1))
	}
	// At 2s, a keyframe arrives — but segment_secs=60 so no roll yet.
	hub.Write(syntheticAU(base.Add(2*time.Second), true, 5))
	// Wait for the writer to process.
	time.Sleep(200 * time.Millisecond)

	// Force a roll by sending a keyframe well past the 60s boundary.
	hub.Write(syntheticAU(base.Add(61*time.Second), true, 5))
	time.Sleep(200 * time.Millisecond)

	cancel()
	<-done

	segs := w.Index().Snapshot()
	if len(segs) != 2 {
		t.Fatalf("expected 2 segments after roll, got %d: %+v", len(segs), segs)
	}
	// First segment should start with a keyframe and have 22 frames.
	if segs[0].Keyframes < 1 {
		t.Fatalf("first segment should contain a keyframe, got %+v", segs[0])
	}
	// Verify the first segment file exists and starts with a 4-byte start code.
	first := filepath.Join(dir, segs[0].File)
	data, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("read first segment: %v", err)
	}
	if len(data) < 4 || data[0] != 0 || data[1] != 0 || data[2] != 0 || data[3] != 1 {
		t.Fatalf("segment should start with 4-byte start code, got %x", data[:4])
	}
	// Sidecar should exist with one entry per frame.
	side, err := os.ReadFile(first + ".ts.jsonl")
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if len(side) == 0 {
		t.Fatalf("sidecar should not be empty")
	}
}

func TestWriter_ActiveFlag(t *testing.T) {
	dir := t.TempDir()
	cfg := testCfg(dir)
	hub := h264.NewAUHubWithSize(64)
	w := NewWriter(hub, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	waitForSubscriber(t, hub)

	hub.Write(syntheticAU(time.Now(), true, 5))
	time.Sleep(100 * time.Millisecond)
	if !w.Active() {
		t.Fatalf("writer should be active while recording")
	}
	cancel()
	<-done
	if w.Active() {
		t.Fatalf("writer should be inactive after shutdown")
	}
}

// --- Reader tests ---

func TestReader_NALUParseRoundtripAndPTS(t *testing.T) {
	dir := t.TempDir()
	cfg := testCfg(dir)
	hub := h264.NewAUHubWithSize(64)
	w := NewWriter(hub, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	waitForSubscriber(t, hub)

	base := time.Now()
	hub.Write(syntheticAU(base, true, 7, 8, 5))
	for i := 1; i < 5; i++ {
		hub.Write(syntheticAU(base.Add(time.Duration(i)*100*time.Millisecond), false, 1))
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	segs := w.Index().Snapshot()
	if len(segs) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segs))
	}
	abs := filepath.Join(dir, segs[0].File)
	r, err := OpenSegment(abs)
	if err != nil {
		t.Fatalf("OpenSegment: %v", err)
	}
	// 5 AUs: keyframe (SPS+PPS+IDR) + 4 non-key.
	var frames int
	var firstKey bool
	var firstPTS time.Duration
	for {
		nalus, pts, key, err := r.Next()
		if errors.Is(err, errEOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if frames == 0 {
			firstKey = key
			firstPTS = pts
			if len(nalus) != 3 {
				t.Fatalf("first AU should have 3 NALUs (SPS+PPS+IDR), got %d", len(nalus))
			}
		}
		frames++
	}
	if frames != 5 {
		t.Fatalf("expected 5 frames, got %d", frames)
	}
	if !firstKey {
		t.Fatalf("first frame should be a keyframe")
	}
	if firstPTS != 0 {
		t.Fatalf("first frame PTS should be 0, got %v", firstPTS)
	}
}

func TestReader_SidecarFallbackFPS(t *testing.T) {
	dir := t.TempDir()
	// Write a raw Annex-B file with 3 NALUs (no sidecar).
	path := filepath.Join(dir, "manual.h264")
	var data []byte
	for _, ty := range []byte{7, 8, 5, 1, 1} {
		data = append(data, startCode...)
		data = append(data, ty, 0x01, 0x02, 0x03)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	r, err := OpenSegment(path)
	if err != nil {
		t.Fatalf("OpenSegment: %v", err)
	}
	if !r.nominal {
		t.Fatalf("expected nominal fallback for missing sidecar")
	}
	var frames int
	var pts time.Duration
	for {
		_, p, _, err := r.Next()
		if errors.Is(err, errEOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		pts = p
		frames++
	}
	if frames != 3 {
		t.Fatalf("expected 3 AUs (SPS+PPS+IDR, then 2 slices), got %d", frames)
	}
	// Last frame at nominal 25fps: index 2 => 80ms.
	if pts != 80*time.Millisecond {
		t.Fatalf("expected nominal PTS 80ms for 3rd frame, got %v", pts)
	}
}
