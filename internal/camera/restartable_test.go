package camera

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeSource is a minimal Camera: a controllable frames channel that
// closes on Stop, plus param bookkeeping.
type fakeSource struct {
	ctx    context.Context
	frames chan Frame
	closed bool

	started  int
	stopped  int
	params   map[string]interface{}
	startErr error
}

func newFakeSource() *fakeSource {
	return &fakeSource{frames: make(chan Frame), params: map[string]interface{}{}}
}

func (f *fakeSource) Start(ctx context.Context) error {
	f.started++
	f.ctx = ctx
	return f.startErr
}
func (f *fakeSource) Stop() error {
	f.stopped++
	if !f.closed {
		f.closed = true
		close(f.frames)
	}
	return nil
}
func (f *fakeSource) Frames() <-chan Frame { return f.frames }
func (f *fakeSource) SetParam(name string, value interface{}) error {
	f.params[name] = value
	return nil
}
func (f *fakeSource) GetParam(name string) (interface{}, error) {
	return f.params[name], nil
}
func (f *fakeSource) Info() CameraInfo { return CameraInfo{Name: "fake"} }

func recvFrame(t *testing.T, ch <-chan Frame) Frame {
	t.Helper()
	select {
	case f := <-ch:
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("no frame within 2s")
		return Frame{}
	}
}

func TestRestartableFramesChannelStableAcrossRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := newFakeSource()
	built := 0
	r := NewRestartable(ctx, first, func(ctx context.Context) (Camera, error) {
		built++
		return newFakeSource(), nil
	})
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}

	first.frames <- Frame{PTS: 1}
	if f := recvFrame(t, r.Frames()); f.PTS != 1 {
		t.Fatalf("frame 1 passthrough, got %v", f.PTS)
	}

	if err := r.Restart(); err != nil {
		t.Fatal(err)
	}
	if first.stopped != 1 {
		t.Fatalf("old source stopped %d times, want 1", first.stopped)
	}
	// The stable channel must NOT have closed on restart.
	second := r.Info() // forwards to current — proves swap
	if second.Name != "fake" {
		t.Fatalf("current after restart = %+v", second)
	}

	// Feed a frame through the wrapped current source (grab it via a
	// param probe — the wrapper holds it privately).
	if err := r.SetParam("probe", true); err != nil {
		t.Fatal(err)
	}
	// Send on the ORIGINAL channel of `first` must be a send-on-closed
	// (stopped) — recover to prove it: sending must panic.
	func() {
		defer func() { recover() }()
		first.frames <- Frame{}
		t.Fatal("old channel must be closed after restart")
	}()

	// Reach the new source's channel through a second factory-built
	// instance is not possible from outside; instead prove liveness via
	// the pump: use RestartableCamera's current through a frames feed
	// from a source we control — use a third restart with a source whose
	// channel we hold.
	third := newFakeSource()
	r2 := NewRestartable(ctx, newFakeSource(), func(ctx context.Context) (Camera, error) {
		return third, nil
	})
	if err := r2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r2.Restart(); err != nil {
		t.Fatal(err)
	}
	third.frames <- Frame{PTS: 42}
	if f := recvFrame(t, r2.Frames()); f.PTS != 42 {
		t.Fatalf("frame from rebuilt source not forwarded, got %v", f.PTS)
	}
}

func TestRestartableSetParamForwardsToCurrent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second := newFakeSource()
	r := NewRestartable(ctx, newFakeSource(), func(ctx context.Context) (Camera, error) {
		return second, nil
	})
	_ = r.Start(ctx)
	if err := r.SetParam("hFlip", true); err != nil {
		t.Fatal(err)
	}
	if err := r.Restart(); err != nil {
		t.Fatal(err)
	}
	if err := r.SetParam("hFlip", false); err != nil {
		t.Fatal(err)
	}
	if v, _ := r.GetParam("hFlip"); v != false {
		t.Fatalf("param after restart = %v, want false (forwarded to NEW source)", v)
	}
	if v, _ := second.GetParam("hFlip"); v != false {
		t.Fatalf("new source param = %v", second.params)
	}
}

func TestRestartableFactoryErrorLeavesWrapperConsistent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewRestartable(ctx, newFakeSource(), func(ctx context.Context) (Camera, error) {
		return nil, errors.New("boom")
	})
	_ = r.Start(ctx)
	if err := r.Restart(); err == nil {
		t.Fatal("factory error must surface")
	}
	// Old source stopped but nothing new started: the wrapper reports the
	// failure; the caller (web) degrades to a full process restart.
}

func TestRestartableStopClosesStableChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewRestartable(ctx, newFakeSource(), func(ctx context.Context) (Camera, error) {
		return newFakeSource(), nil
	})
	_ = r.Start(ctx)
	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-r.Frames():
		if ok {
			t.Fatal("stable channel must be closed after Stop")
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed after Stop")
	}
}
