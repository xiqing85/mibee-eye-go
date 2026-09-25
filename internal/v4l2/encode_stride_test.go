//go:build amd64 || arm64

package v4l2

import (
	"bytes"
	"testing"
)

// The bcm2835 M2M encoder negotiates OUTPUT bytesperline = ALIGN(width, 64)
// regardless of the requested value (verified on hardware: 720 -> 768,
// 480 -> 512; 1280 stays 1280). Frames must be laid out in that padded
// stride or the driver misreads every row past the first — on-stream this
// shows up as garbled macroblocks ("花屏") for any width not a multiple
// of 64, i.e. exactly the 90°/270° rotation cases.

// 4x2 I420, contiguous. Y = 8 bytes, U = 2, V = 2.
var contiguousFrame = []byte{
	1, 2, 3, 4, // Y row 0
	5, 6, 7, 8, // Y row 1
	0x10, 0x11, // U
	0x20, 0x21, // V
}

func TestCopyPaddedI420PadsRows(t *testing.T) {
	// Driver-negotiated stride 6 (ALIGN(4, 64) is hypothetical here; the
	// layout contract is what matters): Y rows at 6, then U at offset
	// 6*2=12 with stride 3, then V at 12 + 3*1 = 15, total 18.
	want := []byte{
		1, 2, 3, 4, 0, 0, // Y row 0 @6
		5, 6, 7, 8, 0, 0, // Y row 1 @6
		0x10, 0x11, 0, // U row @3
		0x20, 0x21, 0, // V row @3
	}
	dst := make([]byte, len(want))
	n := copyPaddedI420(dst, contiguousFrame, 4, 2, 6)
	if n != len(want) {
		t.Fatalf("written = %d, want %d", n, len(want))
	}
	if !bytes.Equal(dst, want) {
		t.Fatalf("padded layout mismatch:\n got %v\nwant %v", dst, want)
	}
}

func TestCopyPaddedI420UnpaddedStrideIsContiguous(t *testing.T) {
	// stride == width: layout must be a plain copy of the frame.
	dst := make([]byte, len(contiguousFrame))
	n := copyPaddedI420(dst, contiguousFrame, 4, 2, 4)
	if n != len(contiguousFrame) {
		t.Fatalf("written = %d, want %d", n, len(contiguousFrame))
	}
	if !bytes.Equal(dst, contiguousFrame) {
		t.Fatalf("unpadded stride must be a verbatim copy:\n got %v\nwant %v", dst, contiguousFrame)
	}
}

func TestCopyPaddedI420RejectsShortBuffer(t *testing.T) {
	dst := make([]byte, 6*2*3/2-1) // one byte short of the padded frame
	if n := copyPaddedI420(dst, contiguousFrame, 4, 2, 6); n != 0 {
		t.Fatalf("short destination must write nothing, wrote %d", n)
	}
}
