package v4l2

import (
	"testing"
	"unsafe"
)

// The struct layouts below are pinned to the 64-bit kernel V4L2 UABI —
// these tests are the contract that keeps the unsafe casts honest.
func TestStructSizes(t *testing.T) {
	if got := unsafe.Sizeof(V4l2Capability{}); got != 104 {
		t.Errorf("V4l2Capability size = %d, want 104", got)
	}
	if got := unsafe.Sizeof(V4l2Format{}); got != 208 {
		t.Errorf("V4l2Format size = %d, want 208", got)
	}
	if got := unsafe.Sizeof(V4l2RequestBuffers{}); got != 20 {
		t.Errorf("V4l2RequestBuffers size = %d, want 20", got)
	}
	if got := unsafe.Sizeof(V4l2Buffer{}); got != 88 {
		t.Errorf("V4l2Buffer size = %d, want 88", got)
	}
	if got := unsafe.Sizeof(V4l2Plane{}); got != 64 {
		t.Errorf("V4l2Plane size = %d, want 64", got)
	}
	if got := unsafe.Sizeof(V4l2PixFormatMplane{}); got != 188 {
		t.Errorf("V4l2PixFormatMplane size = %d, want 188 (packed)", got)
	}
	if got := unsafe.Sizeof(V4l2PixFormat{}); got != 48 {
		t.Errorf("V4l2PixFormat size = %d, want 48", got)
	}
	if got := unsafe.Sizeof(v4l2ExtControl{}); got != 20 {
		t.Errorf("v4l2ExtControl size = %d, want 20 (packed UABI)", got)
	}
	if got := unsafe.Sizeof(v4l2ExtControls{}); got != 32 {
		t.Errorf("v4l2ExtControls size = %d, want 32", got)
	}
}

// v4l2_ext_control's value union is __attribute__((packed)) — on the 64-bit
// UABI it sits at offset 12, NOT natural int64 alignment (16). Writing the
// value at the wrong offset makes the driver read the alignment padding
// (zero): bitrate gets clamped to the driver minimum and GOP/I_PERIOD/
// REPEAT_SEQ_HEADER all silently no-op while FORCE_KEY_FRAME (a button —
// value ignored) keeps working, masking the breakage (found live on
// bcm2835: 0.12 Mbps streams at a 2 Mbps config).
func TestV4l2ExtControlValueOffset(t *testing.T) {
	var c v4l2ExtControl
	if got := unsafe.Offsetof(c.Value); got != 12 {
		t.Errorf("v4l2ExtControl.Value offset = %d, want 12 (packed union)", got)
	}
	var cs v4l2ExtControls
	if got := unsafe.Offsetof(cs.Controls); got != 24 {
		t.Errorf("v4l2ExtControls.Controls offset = %d, want 24", got)
	}
}

// Field offsets that matter for unions shared between views.
func TestV4l2BufferOffsets(t *testing.T) {
	var b V4l2Buffer
	b.SetMemOffset(0x12345678)
	if got := uint32(b.M[0]) | uint32(b.M[1])<<8 | uint32(b.M[2])<<16 | uint32(b.M[3])<<24; got != 0x12345678 {
		t.Errorf("SetMemOffset roundtrip = %#x", got)
	}
	var p V4l2Plane
	p.SetMemOffset(0x0A0B0C0D)
	if p.memOffset() != 0x0A0B0C0D {
		t.Errorf("plane memOffset = %#x", p.memOffset())
	}
}

func TestIocEncoding(t *testing.T) {
	// VIDIOC_QUERYCAP for the 104-byte capability struct on Linux:
	// dir=READ(2) type='V'(86) nr=0 size=104
	want := uint32(2)<<30 | 104<<16 | uint32('V')<<8 | 0
	if vidiocQuerycap != want {
		t.Errorf("vidiocQuerycap = %#x, want %#x", vidiocQuerycap, want)
	}
	// VIDIOC_STREAMON: dir=WRITE(1) nr=18 size=4
	wantOn := uint32(1)<<30 | 4<<16 | uint32('V')<<8 | 18
	if vidiocStreamon != wantOn {
		t.Errorf("vidiocStreamon = %#x, want %#x", vidiocStreamon, wantOn)
	}
}

func TestFourcc(t *testing.T) {
	if FourccYU12 != 0x32315559 { // "YU12" little-endian
		t.Errorf("FourccYU12 = %#x", FourccYU12)
	}
	if FourccH264 != 0x34363248 { // "H264"
		t.Errorf("FourccH264 = %#x", FourccH264)
	}
}

func TestCstr(t *testing.T) {
	if got := cstr([]byte("bcm2835-codec\x00tail")); got != "bcm2835-codec" {
		t.Errorf("cstr = %q", got)
	}
	if got := cstr([]byte("nodefault")); got != "nodefault" {
		t.Errorf("cstr no-nul = %q", got)
	}
}

// The union views must overlap: writing through PixFormat must be visible
// through the raw backing array and back.
func TestFormatUnionOverlap(t *testing.T) {
	var f V4l2Format
	f.Type = BufTypeVideoCapture
	f.PixFormat().Width = 1920
	f.PixFormat().Height = 1080
	if f.PixFormat().Width != 1920 || f.PixFormat().Height != 1080 {
		t.Fatalf("pix view roundtrip failed")
	}
	// First union word now holds width — verify through the raw array.
	if uint32(f.Fmt[0]) != 1920 {
		t.Errorf("union overlap broken: Fmt[0] = %#x", uint32(f.Fmt[0]))
	}

	var m V4l2Format
	m.Type = BufTypeVideoOutputMplane
	m.Mplane().NumPlanes = 1
	if m.Mplane().NumPlanes != 1 {
		t.Fatalf("mplane view roundtrip failed")
	}
}

// VIDIOC_S_CTRL (nr 3, struct v4l2_control = 8 bytes) and the
// FORCE_KEY_FRAME CID are pinned to the 64-bit UABI values — the
// RequestKeyframe path depends on both.
func TestVidiocSCtrlAndForceKeyFrameCID(t *testing.T) {
	if vidiocSCtrl != ioc(_IOCWrite, 3, 8) {
		t.Fatalf("vidiocSCtrl = %#x", vidiocSCtrl)
	}
	// V4L2_CID_MPEG_VIDEO_FORCE_KEY_FRAME = V4L2_CID_CODEC_BASE+229
	// (V4L2_CTRL_CLASS_CODEC|0x900 = 0x990900).
	if cidForceKeyFrame != 0x990900+229 {
		t.Fatalf("cidForceKeyFrame = %#x", cidForceKeyFrame)
	}
}

// Buffer-type enum values are pinned to the 64-bit UABI: the M2M encoder
// opens CAPTURE_MPLANE after OUTPUT_MPLANE, and a wrong constant there is
// invisible to struct-size tests — it surfaced live as bcm2835 rejecting
// every CAPTURE S_FMT with EINVAL (13 is SDR_CAPTURE, not 9).
func TestBufferTypeEnumValues(t *testing.T) {
	if BufTypeVideoCapture != 1 {
		t.Errorf("CAPTURE = %d, want 1", BufTypeVideoCapture)
	}
	if BufTypeVideoOutput != 2 {
		t.Errorf("OUTPUT = %d, want 2", BufTypeVideoOutput)
	}
	if BufTypeVideoCaptureMplane != 9 {
		t.Errorf("CAPTURE_MPLANE = %d, want 9", BufTypeVideoCaptureMplane)
	}
	if BufTypeVideoOutputMplane != 10 {
		t.Errorf("OUTPUT_MPLANE = %d, want 10", BufTypeVideoOutputMplane)
	}
}
