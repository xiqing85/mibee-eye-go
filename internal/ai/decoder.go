package ai

// Keyframe decoder: pipes the H.264 access-unit stream into an ffmpeg
// subprocess that decodes keyframes only (-skip_frame nokey) into small
// RGB24 frames for inference.
//
// This never touches the capture/encode path — it is a passive AUHub
// subscriber. ffmpeg is spawned exactly like the other camera helpers
// (rpicam-vid): separate process group, restarted with backoff if it dies.
//
// ffmpeg invocation:
//
//	ffmpeg -nostdin -hide_banner -loglevel error \
//	  -skip_frame nokey -f h264 -i pipe:0 \
//	  -vf scale=320:240 -pix_fmt rgb24 -f rawvideo pipe:1
//
// Only keyframes are decoded (an IDR is a complete I-frame), so the CPU
// cost is one small decode per GOP instead of a continuous decode of the
// full frame rate. Detection cadence therefore follows the camera's
// intra period (camera.idr_period).

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"syscall"
	"time"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/h264"
)

// Decoder output geometry (fixed: small enough for NanoDet input, aspect
// preserved; preprocess resizes to the square model input).
const (
	decoderFrameW = 320
	decoderFrameH = 240
	frameBytes    = decoderFrameW * decoderFrameH * 3
)

// FrameDecoder subscribes to the AUHub and converts the H.264 stream into
// decoded RGB24 keyframes via an ffmpeg subprocess.
type FrameDecoder struct {
	hub     *h264.AUHub
	binPath string
	frames  chan Frame
}

// NewFrameDecoder creates a decoder for the given hub. Call Start to begin.
func NewFrameDecoder(hub *h264.AUHub, binPath string) *FrameDecoder {
	return &FrameDecoder{
		hub:     hub,
		binPath: binPath,
		frames:  make(chan Frame, 2),
	}
}

// Frames returns the decoded-frame channel (drop-oldest under
// backpressure: the consumer holds the latest frames, slow inference must
// never stall ffmpeg).
func (d *FrameDecoder) Frames() <-chan Frame { return d.frames }

// Start runs the decode loop until ctx is cancelled. It spawns ffmpeg,
// feeds it access units from the hub, and reads fixed-size RGB24 frames.
// A dying ffmpeg (e.g. malformed input after a restart) is respawned with
// backoff, mirroring the camera subprocess pattern.
func (d *FrameDecoder) Start(ctx context.Context) {
	go func() {
		defer close(d.frames)
		backoff := time.Second
		const maxBackoff = 30 * time.Second
		for {
			if ctx.Err() != nil {
				return
			}
			if err := d.runOnce(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("ai: ffmpeg decoder exited, restarting", "error", err, "backoff", backoff)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
					backoff = min(backoff*2, maxBackoff)
				}
				continue
			}
			backoff = time.Second
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
}

// runOnce spawns one ffmpeg process and pumps data until it exits.
func (d *FrameDecoder) runOnce(ctx context.Context) error {
	sub := d.hub.Subscribe(ctx)

	cmd := exec.CommandContext(ctx, d.binPath, decoderArgs()...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		d.hub.Unsubscribe(sub.ID)
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		d.hub.Unsubscribe(sub.ID)
		return fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		d.hub.Unsubscribe(sub.ID)
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	// Writer: serialize access units to Annex-B and push into ffmpeg.
	writeErr := make(chan error, 1)
	go func() {
		defer stdin.Close()
		for au := range sub.Channel {
			if _, err := stdin.Write(annexB(au)); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- nil
	}()

	readErr := make(chan error, 1)
	go func() {
		readErr <- readFrames(stdout, d.frames)
	}()

	waitErr := cmd.Wait()

	// Close the subscription so the writer's range terminates; drain it
	// so the hub never blocks on a full buffer for a dead consumer.
	d.hub.Unsubscribe(sub.ID)
	for range sub.Channel {
	}
	if err := <-writeErr; err != nil && ctx.Err() == nil {
		return fmt.Errorf("feed ffmpeg: %w", err)
	}
	if err := <-readErr; err != nil && ctx.Err() == nil {
		return err
	}
	return waitErr
}

// decoderArgs builds the ffmpeg argument vector.
func decoderArgs() []string {
	return []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		// Decode keyframes only: one small decode per GOP.
		"-skip_frame", "nokey",
		"-f", "h264", "-i", "pipe:0",
		"-vf", fmt.Sprintf("scale=%d:%d", decoderFrameW, decoderFrameH),
		"-pix_fmt", "rgb24",
		"-f", "rawvideo", "pipe:1",
	}
}

// annexB serializes an access unit with 4-byte start codes.
func annexB(au h264.AccessUnit) []byte {
	size := 0
	for _, n := range au.NALUs {
		size += 4 + len(n.Data)
	}
	buf := make([]byte, 0, size)
	for _, n := range au.NALUs {
		buf = append(buf, 0x00, 0x00, 0x00, 0x01)
		buf = append(buf, n.Data...)
	}
	return buf
}

// readFrames reads fixed-size RGB24 frames from r and forwards them to
// out (non-blocking; drops when the consumer lags).
func readFrames(r io.Reader, out chan<- Frame) error {
	reader := bufio.NewReaderSize(r, frameBytes)
	buf := make([]byte, frameBytes)
	for {
		if _, err := io.ReadFull(reader, buf); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("read frame: %w", err)
		}
		frame := Frame{Width: decoderFrameW, Height: decoderFrameH, Data: append([]byte(nil), buf...)}
		select {
		case out <- frame:
		default: // consumer busy — drop this frame, keep the next
		}
	}
}
