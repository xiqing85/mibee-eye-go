package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xiqing85/mibee-eye-go/internal/h264"
)

func au(key bool) h264.AccessUnit {
	return h264.AccessUnit{KeyFrame: key, NALUs: []h264.NALU{{Data: []byte{0x1}}}}
}

// TestMseTimeline_HandoffAcrossConnections: a second connection's first
// timestamp must continue past the last one any earlier connection emitted
// — the seamless-reconnect contract (SPEC §4.1): a client that transparently
// refetches keeps appending to its existing SourceBuffer, so the timeline
// must never restart at zero.
func TestMseTimeline_HandoffAcrossConnections(t *testing.T) {
	base := time.Now()
	c1 := newMseTimeline()
	var last uint64
	for i := 0; i < 5; i++ {
		ts, _ := c1.next(base.Add(time.Duration(i) * 66 * time.Millisecond))
		if ts <= last && i > 0 {
			t.Fatalf("connection 1 timestamps must increase: %d after %d", ts, last)
		}
		last = ts
	}
	c2 := newMseTimeline()
	first2, _ := c2.next(base.Add(10 * time.Second))
	if first2 <= last {
		t.Fatalf("second connection must seed past first's last timestamp: %d <= %d", first2, last)
	}
	_, d := c2.next(base.Add(10*time.Second + 66*time.Millisecond))
	if d == 0 {
		t.Fatal("connection 2 durations must stay positive after handoff")
	}
}

// TestMseTimeline_ClampsStall: a long producer stall (server realignment
// waiting for an IDR) clamps to 200 ms of media time so the client timeline
// rides through the skip without a buffered-range hole.
func TestMseTimeline_ClampsStall(t *testing.T) {
	base := time.Now()
	tl := newMseTimeline()
	tl.next(base)
	_, d := tl.next(base.Add(5 * time.Second))
	if d != 18000 {
		t.Fatalf("long stall must clamp to 18000 ticks (200ms), got %d", d)
	}
	_, d = tl.next(base.Add(5*time.Second + 66*time.Millisecond))
	if d < 90 {
		t.Fatalf("normal frame duration must be ≥90 ticks, got %d", d)
	}
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

// streamBeyondDeadline mounts a chunked handler — through the REAL
// middleware chain (observeMiddleware wraps the ResponseWriter, and an
// Unwrap-less wrapper would swallow ResponseController deadline control) —
// on a server with the given WriteTimeout, and reports how many of the
// paced chunks the client received.
func streamBeyondDeadline(t *testing.T, clear bool) int {
	t.Helper()
	s := &Server{observe: NewObserve()}
	srv := httptest.NewUnstartedServer(s.observeMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		if clear {
			clearWriteDeadline(w)
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte{byte(i)})
			flusher.Flush()
			time.Sleep(60 * time.Millisecond)
		}
	})))
	srv.Config.WriteTimeout = 100 * time.Millisecond
	srv.Start()
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 8)
	n, _ := io.ReadFull(resp.Body, buf)
	return n
}

// TestStreamingHandlersOutliveWriteTimeout: chunked streams (MSE, SSE) are
// long-lived by design; the global http.Server.WriteTimeout (default 30s)
// must not cut them off — every 30s cutoff forced the player into a full
// black-flash reconnect cycle.
func TestStreamingHandlersOutliveWriteTimeout(t *testing.T) {
	if got := streamBeyondDeadline(t, true); got != 3 {
		t.Fatalf("deadline-cleared stream must deliver all chunks, got %d/3", got)
	}
}

// TestWriteTimeoutStillAppliesToPlainHandlers documents the other half of
// the contract: the global deadline keeps guarding non-streaming routes.
func TestWriteTimeoutStillAppliesToPlainHandlers(t *testing.T) {
	if got := streamBeyondDeadline(t, false); got >= 3 {
		t.Fatalf("plain handler must still be cut by WriteTimeout, got %d/3 chunks", got)
	}
}
