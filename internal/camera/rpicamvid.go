// rpicamvid.go — camera capture via the system rpicam-vid subprocess.
// Alternative to mtxrpicam for Pis where the bundled libcamera is
// ABI-incompatible with the system libcamera (mtxrpicam segfaults).
//
// rpicam-vid writes raw H.264 Annex-B to stdout when invoked with `-o -`.
// The stream is split into NAL units with h264.Parser and grouped into
// access units: with --inline, rpicam-vid emits `SPS PPS IDR <slice> ...`
// where each slice is one complete frame.
package camera

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/h264"
)

// RPiCamVidCamera implements Camera by spawning the system rpicam-vid
// binary and reading H.264 Annex-B NAL units from its stdout.
// If the subprocess dies, it is automatically restarted with exponential
// backoff (1s → 30s max).
type RPiCamVidCamera struct {
	mu     sync.RWMutex
	params Params
	info   CameraInfo

	// subprocess management
	cmd    *exec.Cmd
	stdout io.ReadCloser

	// frame delivery (created once in Start, closed only in Stop)
	framesCh chan Frame
	stopOnce sync.Once

	// lifecycle state
	ctx     context.Context
	cancel  context.CancelFunc
	started bool
	stopped bool
	wg      sync.WaitGroup // tracks run() goroutine

	// binary path and options
	binPath         string
	rotation        int
	frameBufferSize int
	maxBackoff      time.Duration

	// frame drop tracking
	droppedFrames atomic.Uint64
}

// RPiCamVidOption configures RPiCamVidCamera behavior.
type RPiCamVidOption func(*RPiCamVidCamera)

// WithVidBinPath sets the path to the rpicam-vid binary.
func WithVidBinPath(path string) RPiCamVidOption {
	return func(c *RPiCamVidCamera) {
		c.binPath = path
	}
}

// WithVidRotation sets the sensor rotation in degrees (0 or 180).
func WithVidRotation(rotation int) RPiCamVidOption {
	return func(c *RPiCamVidCamera) {
		c.rotation = rotation
	}
}

// WithVidFrameBufferSize sets the frame channel buffer capacity.
func WithVidFrameBufferSize(size int) RPiCamVidOption {
	return func(c *RPiCamVidCamera) {
		c.frameBufferSize = size
	}
}

// WithVidMaxBackoff sets the maximum subprocess restart backoff duration.
func WithVidMaxBackoff(backoff time.Duration) RPiCamVidOption {
	return func(c *RPiCamVidCamera) {
		c.maxBackoff = backoff
	}
}

// WithVidParams sets initial camera parameters.
func WithVidParams(p Params) RPiCamVidOption {
	return func(c *RPiCamVidCamera) {
		c.params = p
	}
}

// WithVidInfo sets camera info metadata.
func WithVidInfo(info CameraInfo) RPiCamVidOption {
	return func(c *RPiCamVidCamera) {
		c.info = info
	}
}

// NewRPiCamVidCamera creates a new RPiCamVidCamera with the given options.
func NewRPiCamVidCamera(opts ...RPiCamVidOption) *RPiCamVidCamera {
	c := &RPiCamVidCamera{
		params:          DefaultParams(),
		binPath:         "rpicam-vid",
		frameBufferSize: 30,
		maxBackoff:      30 * time.Second,
		info: CameraInfo{
			Name:         "RPi Camera",
			Manufacturer: "Raspberry Pi",
			Model:        "IMX219",
			Width:        1280,
			Height:       720,
			FPS:          15,
			Codec:        "H264",
		},
	}

	for _, opt := range opts {
		opt(c)
	}

	c.info.Width = c.params.Width
	c.info.Height = c.params.Height
	c.info.FPS = c.params.FPS

	return c
}

// Start begins capturing frames. The rpicam-vid subprocess is automatically
// restarted if it dies. Call Stop() to shut down.
func (c *RPiCamVidCamera) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.started {
		return fmt.Errorf("camera already started")
	}

	// Validate binary exists and is executable.
	if _, err := exec.LookPath(c.binPath); err != nil {
		return fmt.Errorf("rpicam-vid binary not found: %w", err)
	}

	c.ctx, c.cancel = context.WithCancel(ctx)
	c.started = true
	c.framesCh = make(chan Frame, c.frameBufferSize)

	c.wg.Add(1)
	go c.run()

	return nil
}

// run is the main loop that spawns and monitors the rpicam-vid subprocess.
// It runs in its own goroutine and handles automatic restart on subprocess
// death with exponential backoff.
func (c *RPiCamVidCamera) run() {
	defer c.wg.Done()

	backoff := time.Second
	maxBackoff := c.maxBackoff

	for {
		// Check if we should stop.
		c.mu.RLock()
		stopped := c.stopped
		c.mu.RUnlock()
		if stopped {
			return
		}

		// Spawn subprocess.
		err := c.spawnSubprocess()
		if err != nil {
			slog.Warn("camera: rpicam-vid spawn failed, retrying", "error", err, "backoff", backoff)
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
				continue
			}
		}

		// Reset backoff on successful spawn.
		backoff = time.Second

		// readLoop blocks until the subprocess exits or stdout closes.
		c.readLoop()

		// Clean up dead subprocess (keep framesCh alive).
		c.cleanupSubprocess()

		// Check if we should stop.
		c.mu.RLock()
		stopped = c.stopped
		c.mu.RUnlock()
		if stopped {
			return
		}

		slog.Warn("camera: rpicam-vid exited, restarting", "backoff", backoff)
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(backoff):
			backoff = min(backoff*2, maxBackoff)
		}
	}
}

// spawnSubprocess starts rpicam-vid with the current params.
func (c *RPiCamVidCamera) spawnSubprocess() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	args := c.buildArgs()
	cmd := exec.CommandContext(c.ctx, c.binPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("create stdout pipe: %w", err)
	}
	cmd.Stderr = newTelemetryFilter(os.Stderr)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	env := []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + os.Getenv("PATH"),
	}
	// Pass through libcamera-related environment variables so the subprocess
	// can find IPA tuning files, module paths, etc.
	for _, key := range []string{"LIBCAMERA_IPA_CONFIG_PATH", "LIBCAMERA_IPA_MODULE_PATH", "LIBCAMERA_LOG_LEVELS"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start rpicam-vid: %w", err)
	}

	c.cmd = cmd
	c.stdout = stdout

	slog.Info("camera: rpicam-vid subprocess started", "pid", cmd.Process.Pid, "args", args)
	return nil
}

// buildArgs builds the rpicam-vid command line from the current params.
func (c *RPiCamVidCamera) buildArgs() []string {
	p := c.params
	args := []string{
		"-t", "0", // run forever
		"--codec", "h264",
		"--width", strconv.FormatUint(uint64(p.Width), 10),
		"--height", strconv.FormatUint(uint64(p.Height), 10),
		"--framerate", strconv.FormatFloat(float64(p.FPS), 'f', -1, 32),
		"-b", strconv.FormatUint(uint64(p.Bitrate), 10),
		"--inline", // SPS/PPS inline before every IDR (critical for RTSP)
		"-o", "-",  // raw H.264 Annex-B to stdout
	}
	// Keyframe interval (also drives the AI keyframe-decode cadence).
	// 0 keeps rpicam-vid's default.
	if p.IDRPeriod > 0 {
		args = append(args, "--intra", strconv.FormatUint(uint64(p.IDRPeriod), 10))
	}
	if c.rotation == 180 {
		args = append(args, "--rotation", "180")
	}
	if p.HFlip {
		args = append(args, "--hflip")
	}
	if p.VFlip {
		args = append(args, "--vflip")
	}
	return args
}

// readLoop reads H.264 Annex-B from the subprocess stdout, splits it into
// NAL units, groups them into access units, and sends each frame to the
// frames channel. Blocks until the subprocess exits or stdout closes.
// Called synchronously from run() — does NOT clean up subprocess on exit.
func (c *RPiCamVidCamera) readLoop() {
	c.mu.RLock()
	stdout := c.stdout
	framesCh := c.framesCh
	c.mu.RUnlock()

	if stdout == nil || framesCh == nil {
		return
	}

	parser := h264.NewParser()
	reader := bufio.NewReaderSize(stdout, 64*1024)

	var pending []byte // partial trailing NALU awaiting more data
	var curAU []h264.NALU
	var curStart time.Time

	buf := make([]byte, 64*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			nalus, rest := extractCompleteNALUs(pending, parser)
			pending = rest

			for _, nalu := range nalus {
				if startsNewAU(nalu, curAU) && len(curAU) > 0 {
					c.sendFrame(framesCh, curAU, curStart)
					curAU = nil
				}
				if len(curAU) == 0 {
					curStart = time.Now()
				}
				curAU = append(curAU, nalu)
			}
		}
		if err != nil {
			if err != io.EOF {
				slog.Error("camera: rpicam-vid stdout read error", "error", err)
			}
			break
		}
	}

	// Flush any remaining access unit.
	if len(curAU) > 0 {
		c.sendFrame(framesCh, curAU, curStart)
	}
}

// sendFrame builds an Annex-B bytestream from an access unit and delivers
// it to the frames channel, dropping the frame if consumers are slow.
func (c *RPiCamVidCamera) sendFrame(framesCh chan<- Frame, nalus []h264.NALU, ts time.Time) {
	size := 0
	for _, n := range nalus {
		size += 4 + len(n.Data)
	}
	data := make([]byte, 0, size)
	for _, n := range nalus {
		data = append(data, 0x00, 0x00, 0x00, 0x01)
		data = append(data, n.Data...)
	}

	frame := Frame{Data: data, Timestamp: ts}
	select {
	case framesCh <- frame:
	default:
		c.droppedFrames.Add(1)
	}
}

// extractCompleteNALUs returns the complete NAL units in data and the
// trailing bytes that may form a partial NALU (kept for the next read).
func extractCompleteNALUs(data []byte, parser *h264.Parser) ([]h264.NALU, []byte) {
	positions := parser.FindStartCodes(data)
	if len(positions) == 0 {
		return nil, data
	}

	var nalus []h264.NALU
	for i := 0; i < len(positions)-1; i++ {
		pos := positions[i]
		naluStart := pos + 4
		if pos+4 > len(data) || data[pos] != 0 || data[pos+1] != 0 || data[pos+2] != 0 || data[pos+3] != 1 {
			naluStart = pos + 3
		}
		naluEnd := positions[i+1]
		if naluStart >= naluEnd {
			continue
		}
		naluData := data[naluStart:naluEnd]
		naluType := naluData[0] & 0x1F
		nalus = append(nalus, h264.NALU{
			Type:  naluType,
			Data:  naluData,
			IsIDR: naluType == 5,
			IsSPS: naluType == 7,
			IsPPS: naluType == 8,
		})
	}

	// Keep the trailing partial NALU (from the last start code onward).
	return nalus, data[positions[len(positions)-1]:]
}

// startsNewAU reports whether nalu begins a new access unit.
// rpicam-vid emits `SPS PPS IDR <slice> <slice> ...` where each slice is
// one complete frame. SPS/PPS/IDR group into the keyframe access unit;
// every other NALU (slices, SEI) begins its own access unit.
func startsNewAU(nalu h264.NALU, cur []h264.NALU) bool {
	switch {
	case nalu.IsSPS:
		return true
	case nalu.IsPPS:
		return !containsType(cur, 7)
	case nalu.IsIDR:
		return !containsType(cur, 7) && !containsType(cur, 8)
	default:
		return true
	}
}

func containsType(nalus []h264.NALU, typ byte) bool {
	for _, n := range nalus {
		if n.Type == typ {
			return true
		}
	}
	return false
}

// Stop gracefully stops the camera and its subprocess.
func (c *RPiCamVidCamera) Stop() error {
	c.stopOnce.Do(func() {
		// Signal run() to stop accepting restarts.
		c.mu.Lock()
		c.stopped = true
		if c.cancel != nil {
			c.cancel()
		}
		c.mu.Unlock()

		// Kill subprocess and close stdout (causes readLoop to exit).
		c.cleanupSubprocess()

		// Close frames channel so downstream consumers exit their range loops.
		c.mu.Lock()
		if c.framesCh != nil {
			close(c.framesCh)
			c.framesCh = nil
		}
		c.mu.Unlock()

		// Wait for run() goroutine to finish (with timeout).
		c.waitForShutdown()
	})

	return nil
}

// waitForShutdown waits for the run() goroutine with a timeout.
func (c *RPiCamVidCamera) waitForShutdown() {
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		slog.Warn("camera: Stop timed out waiting for run() goroutine after 10s")
	}
}

// cleanupSubprocess cleans up the rpicam-vid subprocess and stdout pipe.
// Does NOT close framesCh — that's only done by Stop().
func (c *RPiCamVidCamera) cleanupSubprocess() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stdout != nil {
		if err := c.stdout.Close(); err != nil {
			slog.Debug("camera: cleanup stdout error", "err", err)
		}
		c.stdout = nil
	}

	if c.cmd != nil && c.cmd.Process != nil {
		if err := c.cmd.Process.Kill(); err != nil {
			slog.Debug("camera: kill rpicam-vid error", "err", err)
		}
		if err := c.cmd.Wait(); err != nil {
			slog.Debug("camera: wait rpicam-vid error", "err", err)
		}
		c.cmd = nil
	}
}

// Frames returns the read-only channel of captured frames.
func (c *RPiCamVidCamera) Frames() <-chan Frame {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.framesCh
}

// SetParam modifies a camera parameter. rpicam-vid has no runtime parameter
// channel, so the value is stored in memory and applied on the next
// subprocess restart.
func (c *RPiCamVidCamera) SetParam(name string, value interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.started || c.stopped {
		return fmt.Errorf("camera not started")
	}

	paramName, ok := mapParamName(name)
	if !ok {
		return fmt.Errorf("unknown parameter: %s", name)
	}

	if err := setParamValue(&c.params, paramName, value); err != nil {
		return fmt.Errorf("set %s: %w", name, err)
	}

	c.info.Width = c.params.Width
	c.info.Height = c.params.Height
	c.info.FPS = c.params.FPS

	return nil
}

// GetParam returns the current value of a camera parameter.
func (c *RPiCamVidCamera) GetParam(name string) (interface{}, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	paramName, ok := mapParamName(name)
	if !ok {
		return nil, fmt.Errorf("unknown parameter: %s", name)
	}

	return getParamValue(c.params, paramName)
}

// Info returns the camera device information.
func (c *RPiCamVidCamera) Info() CameraInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.info
}

// DroppedFrames returns the count of frames dropped due to slow consumers.
func (c *RPiCamVidCamera) DroppedFrames() uint64 {
	return c.droppedFrames.Load()
}
