// Package recording implements the local H.264 recording subsystem:
// a Writer that subscribes to the camera AUHub and accumulates access
// units into Annex-B segment files, an Index over those segments, a
// Retention policy that prunes old/oversized recordings, and a Reader
// that parses segments back into access-unit-shaped frames for playback.
package recording

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// SegmentInfo describes one recorded segment file.
// JSON field order is fixed by the struct field order (encoding/json).
type SegmentInfo struct {
	File      string `json:"file"`      // path relative to the recording root
	StartMS   int64  `json:"start_ms"`  // unix milliseconds of first frame
	EndMS     int64  `json:"end_ms"`    // unix milliseconds of last frame
	Size      int64  `json:"size"`      // segment file size in bytes
	Frames    int    `json:"frames"`    // number of access units
	Keyframes int    `json:"keyframes"` // number of keyframe access units
}

// Index is an in-memory view of the recording index file.
// It is safe for concurrent use.
type Index struct {
	mu       sync.RWMutex
	path     string // absolute path to index.jsonl
	segments []SegmentInfo
}

// NewIndex returns an empty Index backed by the given index file path.
func NewIndex(path string) *Index {
	return &Index{path: path}
}

// Load reads the index file at path, skipping corrupt lines and
// tolerating a missing file (returns an empty index).
func (ix *Index) Load(path string) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.path = path
	ix.segments = ix.segments[:0]

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("opening index %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var si SegmentInfo
		if err := json.Unmarshal([]byte(line), &si); err != nil {
			// Tolerate and skip corrupt lines.
			continue
		}
		ix.segments = append(ix.segments, si)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading index %s: %w", path, err)
	}
	return nil
}

// Append adds a segment record to the in-memory index and appends one
// line to the index file. The file is created if it does not exist.
func (ix *Index) Append(si SegmentInfo) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	line, err := json.Marshal(si)
	if err != nil {
		return fmt.Errorf("marshaling index record: %w", err)
	}

	f, err := os.OpenFile(ix.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening index %s for append: %w", ix.path, err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("appending index record: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing index %s: %w", ix.path, err)
	}

	ix.segments = append(ix.segments, si)
	return nil
}

// Lookup returns segments overlapping [startMs, endMs], inclusive.
// An empty range (startMs > endMs) returns nil.
func (ix *Index) Lookup(startMs, endMs int64) []SegmentInfo {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if startMs > endMs {
		return nil
	}
	var out []SegmentInfo
	for _, si := range ix.segments {
		if si.EndMS >= startMs && si.StartMS <= endMs {
			out = append(out, si)
		}
	}
	return out
}

// Remove deletes a segment record from the in-memory index and rewrites
// the index file without it. It is used by retention after deleting the
// underlying file.
func (ix *Index) Remove(file string) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	kept := ix.segments[:0]
	for _, si := range ix.segments {
		if si.File != file {
			kept = append(kept, si)
		}
	}
	ix.segments = kept

	return ix.rewriteLocked()
}

// rewriteLocked rewrites the index file to match the in-memory segments.
// Caller must hold ix.mu.
func (ix *Index) rewriteLocked() error {
	tmp := ix.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("creating index temp %s: %w", tmp, err)
	}
	w := bufio.NewWriter(f)
	for _, si := range ix.segments {
		line, err := json.Marshal(si)
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("marshaling index record: %w", err)
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("writing index temp: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("flushing index temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("syncing index temp: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("closing index temp: %w", err)
	}
	if err := os.Rename(tmp, ix.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming index temp: %w", err)
	}
	return nil
}

// Snapshot returns a copy of the current segments, sorted by StartMS.
func (ix *Index) Snapshot() []SegmentInfo {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]SegmentInfo, len(ix.segments))
	copy(out, ix.segments)
	sort.Slice(out, func(i, j int) bool { return out[i].StartMS < out[j].StartMS })
	return out
}

// TotalSize returns the sum of segment sizes in bytes.
func (ix *Index) TotalSize() int64 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	var total int64
	for _, si := range ix.segments {
		total += si.Size
	}
	return total
}

// Root returns the recording root directory (the parent of the index file).
func (ix *Index) Root() string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return filepath.Dir(ix.path)
}
