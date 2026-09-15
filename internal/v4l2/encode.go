//go:build amd64 || arm64

package v4l2

import (
	"bytes"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// M2MEncoder drives a V4L2 M2M H.264 encoder node (MPLANE) — e.g.
// bcm2835-codec-encode on Raspberry Pi (/dev/video11) or an i.MX coda
// node. Raw I420 in, Annex-B out. Single-threaded: Encode is the only
// public method and must not be called concurrently.
type M2MEncoder struct {
	file       *os.File
	width      uint32
	height     uint32
	outputBufs [][]byte
	capBufs    [][]byte
	nextOut    uint32
	closed     bool
}

const annexbStartCode = "\x00\x00\x00\x01"

// OpenM2MEncoder opens `path` and configures an H.264 encoder for
// YUV420 input at w×h. Profile/bitrate follow the driver defaults;
// codecs like bcm2835 start at a sane default and can be tuned through
// extended controls, which is deliberately out of scope for v1.
func OpenM2MEncoder(path string, width, height uint32) (*M2MEncoder, error) {
	file, err := openDevice(path)
	if err != nil {
		return nil, fmt.Errorf("v4l2: open encoder %s: %w", path, err)
	}
	e := &M2MEncoder{file: file, width: width, height: height}

	// OUTPUT (raw YUV in), MPLANE, 1 plane holding the full frame.
	var outFmt V4l2Format
	outFmt.Type = BufTypeVideoOutputMplane
	mp := outFmt.Mplane()
	mp.Width = width
	mp.Height = height
	mp.PixelFormat = FourccYU12
	mp.Field = FieldNone
	mp.NumPlanes = 1
	mp.PlaneFmt[0].SizeImage = width * height * 3 / 2
	if err := ioctl(file.Fd(), vidiocSFmt, unsafe.Pointer(&outFmt)); err != nil {
		file.Close()
		return nil, fmt.Errorf("v4l2: S_FMT encoder output: %w", err)
	}

	// CAPTURE (H.264 out), MPLANE, 1 plane, driver-chosen size.
	var capFmt V4l2Format
	capFmt.Type = BufTypeVideoCaptureMplane
	cp := capFmt.Mplane()
	cp.PixelFormat = FourccH264
	cp.Field = FieldNone
	cp.NumPlanes = 1
	if err := ioctl(file.Fd(), vidiocSFmt, unsafe.Pointer(&capFmt)); err != nil {
		file.Close()
		return nil, fmt.Errorf("v4l2: S_FMT encoder capture: %w", err)
	}

	var errOut, errCap error
	e.outputBufs, errOut = e.setupQueue(BufTypeVideoOutputMplane, 4)
	e.capBufs, errCap = e.setupQueue(BufTypeVideoCaptureMplane, 4)
	if errOut != nil {
		e.Close()
		return nil, errOut
	}
	if errCap != nil {
		e.Close()
		return nil, errCap
	}

	onOut := int32(BufTypeVideoOutputMplane)
	if err := ioctl(file.Fd(), vidiocStreamon, unsafe.Pointer(&onOut)); err != nil {
		e.Close()
		return nil, fmt.Errorf("v4l2: STREAMON encoder output: %w", err)
	}
	onCap := int32(BufTypeVideoCaptureMplane)
	if err := ioctl(file.Fd(), vidiocStreamon, unsafe.Pointer(&onCap)); err != nil {
		e.Close()
		return nil, fmt.Errorf("v4l2: STREAMON encoder capture: %w", err)
	}
	return e, nil
}

func (e *M2MEncoder) setupQueue(bufType uint32, count uint32) ([][]byte, error) {
	req := V4l2RequestBuffers{Count: count, Type: bufType, Memory: MemoryMmap}
	if err := ioctl(e.file.Fd(), vidiocReqbufs, unsafe.Pointer(&req)); err != nil {
		return nil, fmt.Errorf("v4l2: encoder REQBUFS(type %d): %w", bufType, err)
	}
	var bufs [][]byte
	for i := uint32(0); i < req.Count; i++ {
		var buf V4l2Buffer
		buf.Index = i
		buf.Type = bufType
		buf.Memory = MemoryMmap
		var planes [1]V4l2Plane
		buf.SetPlanesPtr(&planes[0])
		if err := ioctl(e.file.Fd(), vidiocQuerybuf, unsafe.Pointer(&buf)); err != nil {
			return nil, fmt.Errorf("v4l2: encoder QUERYBUF %d: %w", i, err)
		}
		mapped, err := mmap(e.file.Fd(), int(planes[0].Length), planes[0].memOffset())
		if err != nil {
			return nil, fmt.Errorf("v4l2: encoder mmap %d: %w", i, err)
		}
		bufs = append(bufs, mapped)
		// Queue CAPTURE buffers immediately; OUTPUT buffers are queued
		// per-frame in Encode.
		if bufType == BufTypeVideoCaptureMplane {
			if err := e.queueBuffer(bufType, i, 0); err != nil {
				return nil, err
			}
		}
	}
	return bufs, nil
}

func (e *M2MEncoder) queueBuffer(bufType uint32, index uint32, bytesUsed uint32) error {
	var buf V4l2Buffer
	buf.Index = index
	buf.Type = bufType
	buf.Memory = MemoryMmap
	buf.BytesUsed = bytesUsed
	buf.Field = FieldNone
	var planes [1]V4l2Plane
	planes[0].BytesUsed = bytesUsed
	planes[0].Length = uint32(len(e.bufferFor(bufType, index)))
	buf.SetPlanesPtr(&planes[0])
	if err := ioctl(e.file.Fd(), vidiocQBuf, unsafe.Pointer(&buf)); err != nil {
		return fmt.Errorf("v4l2: encoder QBUF(type %d, idx %d): %w", bufType, index, err)
	}
	return nil
}

func (e *M2MEncoder) bufferFor(bufType uint32, index uint32) []byte {
	if bufType == BufTypeVideoOutputMplane {
		return e.outputBufs[index]
	}
	return e.capBufs[index]
}

// Encode compresses one I420 frame and returns the H.264 Annex-B bytes
// for that frame (may contain several NALUs, SPS/PPS on keyframes).
func (e *M2MEncoder) Encode(yuv []byte) ([]byte, error) {
	if e.closed {
		return nil, fmt.Errorf("v4l2: encoder closed")
	}
	want := e.width * e.height * 3 / 2
	if uint32(len(yuv)) < want {
		return nil, fmt.Errorf("v4l2: short frame: got %d bytes, need %d", len(yuv), want)
	}

	out := e.outputBufs[e.nextOut%uint32(len(e.outputBufs))]
	copy(out, yuv[:want])
	if err := e.queueBuffer(BufTypeVideoOutputMplane, e.nextOut%uint32(len(e.outputBufs)), want); err != nil {
		return nil, err
	}
	e.nextOut++

	var buf V4l2Buffer
	buf.Type = BufTypeVideoCaptureMplane
	buf.Memory = MemoryMmap
	var planes [1]V4l2Plane
	buf.SetPlanesPtr(&planes[0])
	if err := ioctl(e.file.Fd(), vidiocDQBuf, unsafe.Pointer(&buf)); err != nil {
		return nil, fmt.Errorf("v4l2: encoder DQBUF: %w", err)
	}
	if int(buf.Index) >= len(e.capBufs) {
		return nil, fmt.Errorf("v4l2: encoder DQBUF out-of-range index %d", buf.Index)
	}
	frame := make([]byte, planes[0].BytesUsed)
	copy(frame, e.capBufs[buf.Index][:planes[0].BytesUsed])
	if err := e.queueBuffer(BufTypeVideoCaptureMplane, buf.Index, 0); err != nil {
		return nil, err
	}
	if !bytes.Contains(frame, []byte(annexbStartCode)) {
		return nil, fmt.Errorf("v4l2: encoder output has no Annex-B start code (%d bytes)", len(frame))
	}
	return frame, nil
}

// Close stops the encoder and releases the device.
// RequestKeyframe asks the driver to encode the next frame as a
// keyframe (V4L2_CID_MPEG_VIDEO_FORCE_KEY_FRAME). Unlike Encode it is
// safe to call from another goroutine: it issues an independent ioctl
// on the fd and the kernel serializes ioctls per open file. Drivers
// without the control return an error — callers decide whether that is
// fatal.
func (e *M2MEncoder) RequestKeyframe() error {
	ctrl := v4l2Control{id: cidForceKeyFrame, value: 1}
	if err := ioctl(e.file.Fd(), vidiocSCtrl, unsafe.Pointer(&ctrl)); err != nil {
		return fmt.Errorf("v4l2: FORCE_KEY_FRAME: %w", err)
	}
	return nil
}

func (e *M2MEncoder) Close() {
	if e.closed || e.file == nil {
		return
	}
	e.closed = true
	offOut := int32(BufTypeVideoOutputMplane)
	_ = ioctl(e.file.Fd(), vidiocStreamoff, unsafe.Pointer(&offOut))
	offCap := int32(BufTypeVideoCaptureMplane)
	_ = ioctl(e.file.Fd(), vidiocStreamoff, unsafe.Pointer(&offCap))
	for _, b := range append(append([][]byte{}, e.outputBufs...), e.capBufs...) {
		_ = syscall.Munmap(b)
	}
	e.outputBufs, e.capBufs = nil, nil
	e.file.Close()
	e.file = nil
}
