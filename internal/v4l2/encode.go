//go:build amd64 || arm64

package v4l2

import (
	"bytes"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// M2MEncoder drives a V4L2 M2M H.264 encoder node (MPLANE) — e.g.
// bcm2835-codec-encode on Raspberry Pi (/dev/video11) or an i.MX coda
// node. Raw I420 in, Annex-B out. Single-threaded: Encode is the only
// public method and must not be called concurrently.
type M2MEncoder struct {
	file       *os.File
	width      uint32
	height     uint32
	stride     uint32
	outputBufs [][]byte
	capBufs    [][]byte
	nextOut    uint32
	// Software IDR cadence: some drivers accept the GOP/I_PERIOD controls
	// but ignore them (bcm2835 does — verified empirically), so the
	// FORCE_KEY_FRAME button is pressed every iPeriod frames instead.
	iPeriod    uint32
	frameCount uint64
	closed     bool
}

// copyPaddedI420 writes a contiguous I420 frame (w×h, stride w) into dst
// using the driver-negotiated luma stride. Drivers may pad the OUTPUT
// stride regardless of what was requested (bcm2835: ALIGN(width, 64)), and
// the padded sizeimage is stride*h*3/2 with chroma stride stride/2.
// Returns the bytes written, or 0 if dst cannot hold the padded frame.
func copyPaddedI420(dst, src []byte, w, h, stride uint32) int {
	if stride == w {
		if len(dst) < len(src) {
			return 0
		}
		copy(dst, src)
		return len(src)
	}
	cstride := stride / 2
	uOff := stride * h
	vOff := uOff + cstride*(h/2)
	total := vOff + cstride*(h/2)
	if uint32(len(dst)) < total || uint32(len(src)) < w*h*3/2 {
		return 0
	}
	for row := uint32(0); row < h; row++ {
		copy(dst[row*stride:row*stride+w], src[row*w:row*w+w])
	}
	srcU := w * h
	for row := uint32(0); row < h/2; row++ {
		copy(dst[uOff+row*cstride:uOff+row*cstride+w/2], src[srcU+row*(w/2):srcU+row*(w/2)+w/2])
	}
	srcV := srcU + (w/2)*(h/2)
	for row := uint32(0); row < h/2; row++ {
		copy(dst[vOff+row*cstride:vOff+row*cstride+w/2], src[srcV+row*(w/2):srcV+row*(w/2)+w/2])
	}
	return int(total)
}

const annexbStartCode = "\x00\x00\x00\x01"

// OpenM2MEncoder opens `path` and configures an H.264 encoder for
// YUV420 input at w×h with the given best-effort controls. Unsupported
// controls are skipped silently — drivers differ (bcm2835 supports
// I_PERIOD + REPEAT_SEQ_HEADER + BITRATE but not FORCE_KEY_FRAME).
func OpenM2MEncoder(path string, width, height uint32, opts M2MEncoderOptions) (*M2MEncoder, error) {
	file, err := openDevice(path)
	if err != nil {
		return nil, fmt.Errorf("v4l2: open encoder %s: %w", path, err)
	}
	// width/height are finalized after OUTPUT S_FMT negotiation below.
	e := &M2MEncoder{file: file, width: width, height: height}

	// OUTPUT (raw YUV in), MPLANE, 1 plane holding the full frame.
	// The driver may adjust width/height/stride on S_FMT — the struct is
	// updated in place, and the CAPTURE side below must use the
	// negotiated values (bcm2835-codec rejects a CAPTURE S_FMT without
	// them; sequence proven against the same driver family by the Rust
	// twin's shiguredo_v4l2 encoder).
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
	negotiatedW := mp.Width
	negotiatedH := mp.Height
	e.width, e.height = negotiatedW, negotiatedH
	// Honor the negotiated luma stride: bcm2835 pads it to ALIGN(width,64)
	// whatever we request, and reading a contiguous frame at a padded
	// stride garbles every row (only visible for widths not 64-aligned —
	// i.e. the 90°/270° rotation cases).
	e.stride = uint32(mp.PlaneFmt[0].BytesPerLine)
	if e.stride < negotiatedW {
		e.stride = negotiatedW
	}

	// CAPTURE (H.264 out), MPLANE, 1 plane. Width/height mirror the
	// negotiated OUTPUT geometry and sizeimage gives the driver a buffer
	// hint (the driver may still grow it) — both are required by
	// bcm2835-codec.
	var capFmt V4l2Format
	capFmt.Type = BufTypeVideoCaptureMplane
	cp := capFmt.Mplane()
	cp.Width = negotiatedW
	cp.Height = negotiatedH
	cp.PixelFormat = FourccH264
	cp.Field = FieldNone
	cp.NumPlanes = 1
	cp.PlaneFmt[0].SizeImage = 512 * 1024
	if err := ioctl(file.Fd(), vidiocSFmt, unsafe.Pointer(&capFmt)); err != nil {
		file.Close()
		return nil, fmt.Errorf("v4l2: S_FMT encoder capture: %w", err)
	}

	// Best-effort control set (drivers reject what they lack; the Rust
	// twin does the same). Must happen BEFORE REQBUFS/STREAMON — several
	// drivers (bcm2835 included) take codec controls only on a fresh,
	// buffer-less context. I_PERIOD/GOP keeps keyframes periodic —
	// without it mid-stream RTSP joins and recording segmentation break.
	setCtrl := func(id uint32, value int32) {
		_ = setExtCtrl(file.Fd(), id, value)
	}
	if opts.Bitrate > 0 {
		setCtrl(cidVideoBitrate, opts.Bitrate)
	}
	if opts.IPeriod > 0 {
		e.iPeriod = uint32(opts.IPeriod)
		setCtrl(cidVideoH264IPeriod, opts.IPeriod)
		setCtrl(cidVideoGopSize, opts.IPeriod)
		setCtrl(cidVideoRepeatSeqHeader, 1)
	} else if opts.RepeatSeqHeader {
		setCtrl(cidVideoRepeatSeqHeader, 1)
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
	// Queue CAPTURE buffers up front (OUTPUT buffers are queued per-frame
	// in Encode). Must run after both slices are assigned — queueBuffer
	// derives the plane length from them.
	for i := range e.capBufs {
		if err := e.queueBuffer(BufTypeVideoCaptureMplane, uint32(i), 0); err != nil {
			e.Close()
			return nil, err
		}
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
		buf.Length = 1 // plane count — required by the MPLANE buffer ioctls
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
	buf.Length = 1 // plane count — required by the MPLANE buffer ioctls
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

	// Press FORCE_KEY_FRAME every iPeriod frames (button control —
	// execute-on-write; the next encoded frame becomes an IDR). Frame 0
	// is an IDR naturally.
	if e.iPeriod > 0 && e.frameCount > 0 && e.frameCount%uint64(e.iPeriod) == 0 {
		_ = setExtCtrl(e.file.Fd(), cidForceKeyFrame, 1)
	}
	e.frameCount++

	out := e.outputBufs[e.nextOut%uint32(len(e.outputBufs))]
	written := copyPaddedI420(out, yuv[:want], e.width, e.height, e.stride)
	if written == 0 {
		return nil, fmt.Errorf("v4l2: encoder output buffer too small for padded stride %d (w=%d h=%d)", e.stride, e.width, e.height)
	}
	if err := e.queueBuffer(BufTypeVideoOutputMplane, e.nextOut%uint32(len(e.outputBufs)), uint32(written)); err != nil {
		return nil, err
	}
	e.nextOut++

	var buf V4l2Buffer
	buf.Type = BufTypeVideoCaptureMplane
	buf.Memory = MemoryMmap
	buf.Length = 1 // plane count — required by the MPLANE buffer ioctls
	var planes [1]V4l2Plane
	buf.SetPlanesPtr(&planes[0])
	// M2M completion dance: the fd is O_NONBLOCK. Wait on POLLIN — the
	// CAPTURE (encoded) side completing means the OUTPUT (raw input)
	// buffer was consumed too; reclaim it or the pool drains and the
	// next QBUF fails with EINVAL after nbuffers frames. (POLLOUT only
	// means "queue not full" on an M2M fd — it is permanently ready
	// here and must not be treated as a completion signal.)
	fds := []unix.PollFd{{Fd: int32(e.file.Fd()), Events: unix.POLLIN}}
	// Retry on EINTR — signals (subprocess reaping on this device's
	// pipeline, GB28181 timers) abort poll mid-wait; a stray EINTR once
	// killed the whole rotation pipeline in the field.
	deadline := time.Now().Add(1000 * time.Millisecond)
	for {
		timeout := int(time.Until(deadline).Milliseconds())
		if timeout < 0 {
			timeout = 0
		}
		n, err := unix.Poll(fds, timeout)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("v4l2: encoder poll: %w", err)
		}
		if n == 0 {
			return nil, fmt.Errorf("v4l2: encoder poll timeout (1000ms)")
		}
		break
	}
	// Reclaim the consumed input buffer (matches the frame we queued;
	// the encoder completes the OUTPUT side no later than its CAPTURE).
	var ob V4l2Buffer
	ob.Type = BufTypeVideoOutputMplane
	ob.Memory = MemoryMmap
	ob.Length = 1
	var oplanes [1]V4l2Plane
	ob.SetPlanesPtr(&oplanes[0])
	if err := ioctl(e.file.Fd(), vidiocDQBuf, unsafe.Pointer(&ob)); err != nil {
		return nil, fmt.Errorf("v4l2: encoder DQBUF(output): %w", err)
	}
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
	if err := setExtCtrl(e.file.Fd(), cidForceKeyFrame, 1); err != nil {
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
