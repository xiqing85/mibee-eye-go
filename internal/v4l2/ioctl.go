// Package v4l2 provides a small pure-Go V4L2 layer: capability probing,
// MMAP video capture (YU12/I420) and M2M H.264 encoding (MPLANE), used by
// the `camera.mode: v4l2` backend.
//
// Layout note: struct sizes/offsets mirror the 64-bit Linux V4L2 UABI
// (x86_64 / aarch64). Go does not add the trailing alignment padding the
// C ABI guarantees, so every struct carries explicit pad fields and a test
// pins unsafe.Sizeof to the kernel values. 32-bit targets (armv7) are
// deliberately excluded via build tags until audited.
package v4l2

import (
	"os"
	"syscall"
	"unsafe"
)

// ioctl direction bits and request encoder (asm-generic).
const (
	_IOCNone  = 0
	_IOCWrite = 1
	_IOCRead  = 2
	_typeV    = uint32('V')
)

func ioc(dir, nr, size uint32) uint32 {
	return dir<<30 | size<<16 | _typeV<<8 | nr
}

// Buffer types / memory / field / fourcc constants.
const (
	BufTypeVideoCapture       = 1
	BufTypeVideoOutput        = 2
	BufTypeVideoOutputMplane  = 10
	BufTypeVideoCaptureMplane = 13

	MemoryMmap = 1

	FieldNone = 1

	FourccYU12 = uint32('Y') | uint32('U')<<8 | uint32('1')<<16 | uint32('2')<<24
	FourccH264 = uint32('H') | uint32('2')<<8 | uint32('6')<<16 | uint32('4')<<24
)

// Capability bits.
const (
	CapVideoCapture   = 0x00000001
	CapVideoOutput    = 0x00000002
	CapVideoM2M       = 0x00000008
	CapVideoM2MMplane = 0x00004000 // 0x2000 is VIDEO_OUTPUT_MPLANE
	CapStreaming      = 0x04000000
)

// ProbeResult describes what a device node can do for encoding.
type ProbeResult struct {
	Path       string
	Driver     string
	M2MCapable bool
}

// V4l2Capability is struct v4l2_capability (104 bytes).
type V4l2Capability struct {
	Driver       [16]byte
	Card         [32]byte
	BusInfo      [32]byte
	Version      uint32
	Capabilities uint32
	DeviceCaps   uint32
	Reserved     [3]uint32
}

// DriverName returns the NUL-terminated driver string.
func (c *V4l2Capability) DriverName() string { return cstr(c.Driver[:]) }

// V4l2PixFormat is struct v4l2_pix_format (single plane).
type V4l2PixFormat struct {
	Width        uint32
	Height       uint32
	PixelFormat  uint32
	Field        uint32
	BytesPerLine uint32
	SizeImage    uint32
	Colorspace   uint32
	Priv         uint32
	Flags        uint32
	Enc          uint32
	Quantization uint32
	XferFunc     uint32
}

// V4l2Format is struct v4l2_format. The kernel union is 200 bytes with
// 8-byte alignment; modeling it as [25]uint64 makes the Go compiler insert
// the same 4 bytes of padding after Type.
type V4l2Format struct {
	Type uint32
	Fmt  [25]uint64
}

// PixFormat returns the single-plane view of the union.
func (f *V4l2Format) PixFormat() *V4l2PixFormat {
	return (*V4l2PixFormat)(unsafe.Pointer(&f.Fmt[0]))
}

// Mplane returns the multi-plane view of the union.
func (f *V4l2Format) Mplane() *V4l2PixFormatMplane {
	return (*V4l2PixFormatMplane)(unsafe.Pointer(&f.Fmt[0]))
}

// v4l2PlanePixFormat is struct v4l2_plane_pix_format (20 bytes).
type v4l2PlanePixFormat struct {
	SizeImage    uint32
	BytesPerLine uint16
	Reserved     [7]uint16
}

// V4l2PixFormatMplane is struct v4l2_pix_format_mplane (packed, 188 bytes).
type V4l2PixFormatMplane struct {
	Width       uint32
	Height      uint32
	PixelFormat uint32
	Field       uint32
	Colorspace  uint32
	PlaneFmt    [8]v4l2PlanePixFormat
	NumPlanes   uint8
	Flags       uint8
	Reserved    [3]uint16
}

// V4l2RequestBuffers is struct v4l2_requestbuffers (20 bytes).
type V4l2RequestBuffers struct {
	Count        uint32
	Type         uint32
	Memory       uint32
	Capabilities uint32
	Flags        uint8
	Reserved     [3]uint8
}

// V4l2Timecode is struct v4l2_timecode (16 bytes).
type V4l2Timecode struct {
	Type     uint32
	Flags    uint32
	Frames   uint8
	Seconds  uint8
	Minutes  uint8
	Hours    uint8
	UserBits [4]uint8
}

// V4l2Buffer is struct v4l2_buffer for MMAP streaming on 64-bit ABI
// (88 bytes). The `m` union (offset / userptr / planes pointer / fd) is
// modeled as [8]byte with accessors; Go would not emit the trailing pad,
// so it is explicit.
type V4l2Buffer struct {
	Index     uint32
	Type      uint32
	BytesUsed uint32
	Flags     uint32
	Field     uint32
	Timestamp syscall.Timeval
	Timecode  V4l2Timecode
	Sequence  uint32
	Memory    uint32
	M         [8]byte
	Length    uint32
	Reserved2 uint32
	RequestFD int32
	_         [4]byte // tail pad to the C ABI size (88)
}

// SetMemOffset stores an MMAP offset into the `m` union.
func (b *V4l2Buffer) SetMemOffset(off uint32) {
	b.M[0] = byte(off)
	b.M[1] = byte(off >> 8)
	b.M[2] = byte(off >> 16)
	b.M[3] = byte(off >> 24)
}

// SetPlanesPtr stores an MPLANE planes-array pointer into the `m` union.
func (b *V4l2Buffer) SetPlanesPtr(p *V4l2Plane) {
	b.M = [8]byte{}
	*(*uintptr)(unsafe.Pointer(&b.M[0])) = uintptr(unsafe.Pointer(p))
}

// V4l2Plane is struct v4l2_plane (64 bytes).
type V4l2Plane struct {
	BytesUsed  uint32
	Length     uint32
	M          [8]byte // union: mem_offset / userptr / fd / fd64
	DataOffset uint32
	Reserved   [11]uint32
}

// SetMemOffset stores an MMAP offset into the plane's `m` union.
func (p *V4l2Plane) SetMemOffset(off uint32) {
	p.M[0] = byte(off)
	p.M[1] = byte(off >> 8)
	p.M[2] = byte(off >> 16)
	p.M[3] = byte(off >> 24)
}

// memOffset reads the mem_offset interpretation of the plane's `m` union.
func (p *V4l2Plane) memOffset() uint32 {
	return uint32(p.M[0]) | uint32(p.M[1])<<8 | uint32(p.M[2])<<16 | uint32(p.M[3])<<24
}

// v4l2Control mirrors `struct v4l2_control` (driver control set).
type v4l2Control struct {
	id    uint32
	value int32
}

// ioctl request codes, computed from struct sizes.
var (
	vidiocQuerycap  = ioc(_IOCRead, 0, uint32(unsafe.Sizeof(V4l2Capability{})))
	vidiocSFmt      = ioc(_IOCRead|_IOCWrite, 5, uint32(unsafe.Sizeof(V4l2Format{})))
	vidiocReqbufs   = ioc(_IOCRead|_IOCWrite, 8, uint32(unsafe.Sizeof(V4l2RequestBuffers{})))
	vidiocQuerybuf  = ioc(_IOCRead|_IOCWrite, 9, uint32(unsafe.Sizeof(V4l2Buffer{})))
	vidiocQBuf      = ioc(_IOCRead|_IOCWrite, 15, uint32(unsafe.Sizeof(V4l2Buffer{})))
	vidiocDQBuf     = ioc(_IOCRead|_IOCWrite, 17, uint32(unsafe.Sizeof(V4l2Buffer{})))
	vidiocStreamon  = ioc(_IOCWrite, 18, 4)
	vidiocStreamoff = ioc(_IOCWrite, 19, 4)
	// struct v4l2_control { u32 id; i32 value } = 8 bytes.
	vidiocSCtrl = ioc(_IOCWrite, 3, 8)
)

// V4L2_CID_MPEG_VIDEO_FORCE_KEY_FRAME (v4l2-controls.h:
// V4L2_CID_CODEC_BASE+229 — the 64-bit UABI value, pinned by test).
const cidForceKeyFrame = 0x9909E5

func ioctl(fd uintptr, req uint32, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(req), uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// openDevice opens a V4L2 device node read/write, non-blocking.
func openDevice(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
}

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// mmap maps length bytes of the device at the given offset.
func mmap(fd uintptr, length int, offset uint32) ([]byte, error) {
	return syscall.Mmap(int(fd), int64(offset), length, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
}
