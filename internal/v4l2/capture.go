//go:build amd64 || arm64

package v4l2

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Capture is an MMAP-streaming YU12 (I420) capture device. It mirrors the
// proven flow of the Rust twin's capture producer: S_FMT → REQBUFS →
// QUERYBUF+MMAP ×N → STREAMON → poll/DQBUF/QBUF loop.
type Capture struct {
	file    *os.File
	buffers [][]byte
	format  V4l2PixFormat
}

// OpenCapture opens `path` and configures YU12 capture at w×h.
func OpenCapture(path string, width, height uint32) (*Capture, error) {
	file, err := openDevice(path)
	if err != nil {
		return nil, fmt.Errorf("v4l2: open %s: %w", path, err)
	}
	c := &Capture{file: file}

	var fmtSet V4l2Format
	fmtSet.Type = BufTypeVideoCapture
	pix := fmtSet.PixFormat()
	pix.Width = width
	pix.Height = height
	pix.PixelFormat = FourccYU12
	pix.Field = FieldNone
	if err := ioctl(file.Fd(), vidiocSFmt, unsafe.Pointer(&fmtSet)); err != nil {
		file.Close()
		return nil, fmt.Errorf("v4l2: S_FMT capture on %s: %w", path, err)
	}
	if pix.PixelFormat != FourccYU12 {
		file.Close()
		return nil, fmt.Errorf("v4l2: %s does not support YU12 capture (got fourcc %#x)", path, pix.PixelFormat)
	}
	c.format = *pix

	const numBuffers = 4
	req := V4l2RequestBuffers{Count: numBuffers, Type: BufTypeVideoCapture, Memory: MemoryMmap}
	if err := ioctl(file.Fd(), vidiocReqbufs, unsafe.Pointer(&req)); err != nil {
		file.Close()
		return nil, fmt.Errorf("v4l2: REQBUFS capture: %w", err)
	}
	if req.Count == 0 {
		file.Close()
		return nil, fmt.Errorf("v4l2: %s granted zero capture buffers", path)
	}

	for i := uint32(0); i < req.Count; i++ {
		var buf V4l2Buffer
		buf.Index = i
		buf.Type = BufTypeVideoCapture
		buf.Memory = MemoryMmap
		if err := ioctl(file.Fd(), vidiocQuerybuf, unsafe.Pointer(&buf)); err != nil {
			c.Close()
			return nil, fmt.Errorf("v4l2: QUERYBUF %d: %w", i, err)
		}
		off := uint32(buf.M[0]) | uint32(buf.M[1])<<8 | uint32(buf.M[2])<<16 | uint32(buf.M[3])<<24
		mapped, err := mmap(file.Fd(), int(buf.Length), off)
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("v4l2: mmap capture buffer %d: %w", i, err)
		}
		c.buffers = append(c.buffers, mapped)
		if err := c.queue(i); err != nil {
			c.Close()
			return nil, err
		}
	}

	on := int32(BufTypeVideoCapture)
	if err := ioctl(file.Fd(), vidiocStreamon, unsafe.Pointer(&on)); err != nil {
		c.Close()
		return nil, fmt.Errorf("v4l2: STREAMON capture: %w", err)
	}
	return c, nil
}

// Format returns the negotiated pixel format.
func (c *Capture) Format() V4l2PixFormat { return c.format }

func (c *Capture) queue(index uint32) error {
	var buf V4l2Buffer
	buf.Index = index
	buf.Type = BufTypeVideoCapture
	buf.Memory = MemoryMmap
	buf.Field = FieldNone
	if err := ioctl(c.file.Fd(), vidiocQBuf, unsafe.Pointer(&buf)); err != nil {
		return fmt.Errorf("v4l2: QBUF capture %d: %w", index, err)
	}
	return nil
}

// ReadFrame blocks until a captured frame is available and returns a view
// into the MMAP buffer (valid until the next ReadFrame call).
func (c *Capture) ReadFrame() ([]byte, error) {
	var buf V4l2Buffer
	buf.Type = BufTypeVideoCapture
	buf.Memory = MemoryMmap

	// The device is opened O_NONBLOCK, so poll first.
	for {
		p := poller{fd: c.file.Fd()}
		ready, err := p.wait()
		if err != nil {
			return nil, err
		}
		if !ready {
			continue
		}
		if err := ioctl(c.file.Fd(), vidiocDQBuf, unsafe.Pointer(&buf)); err != nil {
			if err == syscall.EAGAIN {
				continue
			}
			return nil, fmt.Errorf("v4l2: DQBUF capture: %w", err)
		}
		break
	}
	if int(buf.Index) >= len(c.buffers) {
		return nil, fmt.Errorf("v4l2: DQBUF returned out-of-range index %d", buf.Index)
	}
	frame := c.buffers[buf.Index][:buf.BytesUsed]
	return frame, c.queue(buf.Index)
}

// Close stops streaming and releases the device.
func (c *Capture) Close() {
	if c.file == nil {
		return
	}
	off := int32(BufTypeVideoCapture)
	_ = ioctl(c.file.Fd(), vidiocStreamoff, unsafe.Pointer(&off))
	for _, b := range c.buffers {
		_ = syscall.Munmap(b)
	}
	c.buffers = nil
	c.file.Close()
	c.file = nil
}

// poller wraps poll(2) for a single fd.
type poller struct{ fd uintptr }

func (p poller) wait() (bool, error) {
	fds := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 1000)
	if err != nil {
		if err == syscall.EINTR {
			return false, nil
		}
		return false, fmt.Errorf("v4l2: poll: %w", err)
	}
	return n > 0, nil
}
