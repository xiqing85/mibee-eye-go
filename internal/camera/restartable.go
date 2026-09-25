// restartable.go provides RestartableCamera — a Camera wrapper whose
// Frames() channel is stable across in-place restarts of the underlying
// source. The web layer uses it to apply geometry-preserving camera
// changes (SPEC §5 applied:"camera_restart") without recycling the whole
// process: stop the old source, build a fresh one from the reloaded
// config, and frames resume on the same channel the AUHub pump reads —
// no consumer sees the swap.
package camera

import (
	"context"
	"fmt"
	"sync"
)

// RestartableCamera wraps a Camera and a factory that builds a fresh
// source from current configuration. The factory must NOT start the
// camera — Restart starts it after the old source released the device
// (V4L2 nodes are exclusive, so the new source can only open once the
// old one stopped).
type RestartableCamera struct {
	mu      sync.Mutex
	current Camera
	factory func(ctx context.Context) (Camera, error)
	ctx     context.Context
	out     chan Frame
	stopped bool
}

// NewRestartable wraps the initial camera (unstarted). factory builds a
// replacement from freshly loaded configuration on Restart.
func NewRestartable(ctx context.Context, initial Camera, factory func(ctx context.Context) (Camera, error)) *RestartableCamera {
	return &RestartableCamera{
		current: initial,
		factory: factory,
		ctx:     ctx,
		out:     make(chan Frame, 4),
	}
}

// Start starts the wrapped camera and begins forwarding its frames.
func (r *RestartableCamera) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return fmt.Errorf("camera: restartable already stopped")
	}
	if err := r.current.Start(ctx); err != nil {
		return err
	}
	go r.pump(r.current)
	return nil
}

// Stop stops the wrapped camera and closes the stable Frames channel —
// the process is going down (graceful shutdown path).
func (r *RestartableCamera) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil
	}
	r.stopped = true
	err := r.current.Stop()
	close(r.out)
	return err
}

// Frames returns the stable, read-only channel. It stays open across
// Restart cycles and closes only on Stop.
func (r *RestartableCamera) Frames() <-chan Frame { return r.out }

// SetParam forwards to the active source (imaging parameter changes,
// GB28181 FrameMirror runtime flips).
func (r *RestartableCamera) SetParam(name string, value interface{}) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current.SetParam(name, value)
}

// GetParam forwards to the active source.
func (r *RestartableCamera) GetParam(name string) (interface{}, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current.GetParam(name)
}

// Info forwards to the active source.
func (r *RestartableCamera) Info() CameraInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current.Info()
}

// Restart swaps the underlying source: stop old (releasing the device),
// build + start a fresh one from the factory. Frames pause for the
// source's stop/start cycle and resume on the same channel. The SPS and
// geometry are the caller's eligibility concern — Restart itself makes
// no assumptions about them.
func (r *RestartableCamera) Restart() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return fmt.Errorf("camera: restartable already stopped")
	}
	if err := r.current.Stop(); err != nil {
		// The old source failed to release; a stale device handle would
		// poison the fresh open, so surface it instead of swapping.
		return fmt.Errorf("camera: stop old source: %w", err)
	}
	fresh, err := r.factory(r.ctx)
	if err != nil {
		return fmt.Errorf("camera: rebuild source: %w", err)
	}
	if err := fresh.Start(r.ctx); err != nil {
		fresh.Stop()
		return fmt.Errorf("camera: start rebuilt source: %w", err)
	}
	r.current = fresh
	go r.pump(fresh)
	return nil
}

// pump forwards one source's frames onto the stable channel until the
// source's channel closes (Stop) or the process context ends.
func (r *RestartableCamera) pump(src Camera) {
	for f := range src.Frames() {
		select {
		case r.out <- f:
		case <-r.ctx.Done():
			return
		}
	}
}
