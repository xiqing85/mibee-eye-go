package ai

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/h264"
)

func TestDecoderArgs(t *testing.T) {
	args := decoderArgs()
	joined := strings.Join(args, " ")
	for _, want := range []string{"-skip_frame nokey", "-f h264 -i pipe:0", "-pix_fmt rgb24", "-f rawvideo pipe:1", "scale=320:240"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	if strings.Contains(joined, "pipe:1") && !strings.Contains(joined, "rawvideo") {
		t.Errorf("output must be rawvideo: %v", args)
	}
}

func TestAnnexB(t *testing.T) {
	au := h264.AccessUnit{
		NALUs: []h264.NALU{
			{Type: 7, Data: []byte{0x67, 0x42}},
			{Type: 5, Data: []byte{0x65, 0x01}},
		},
		KeyFrame: true,
	}
	got := annexB(au)
	want := []byte{0, 0, 0, 1, 0x67, 0x42, 0, 0, 0, 1, 0x65, 0x01}
	if !bytes.Equal(got, want) {
		t.Fatalf("annexB = %v, want %v", got, want)
	}
}

func TestReadFramesSplitsExactChunks(t *testing.T) {
	frameCount := 3
	var buf bytes.Buffer
	pixel := func(i int) byte { return byte(i%251 + 1) }
	for f := 0; f < frameCount; f++ {
		for i := 0; i < frameBytes; i++ {
			buf.WriteByte(pixel(f*frameBytes + i))
		}
	}

	out := make(chan Frame, frameCount)
	if err := readFrames(&buf, out); err != nil {
		t.Fatalf("readFrames: %v", err)
	}
	close(out)

	n := 0
	for frame := range out {
		if frame.Width != decoderFrameW || frame.Height != decoderFrameH {
			t.Fatalf("frame %d geometry = %dx%d", n, frame.Width, frame.Height)
		}
		if len(frame.Data) != frameBytes {
			t.Fatalf("frame %d size = %d", n, len(frame.Data))
		}
		if frame.Data[0] != pixel(n*frameBytes) {
			t.Fatalf("frame %d starts with wrong byte (chunk split broken)", n)
		}
		n++
	}
	if n != frameCount {
		t.Fatalf("frames = %d, want %d", n, frameCount)
	}
}

func TestReadFramesPartialFrameIsNotEmitted(t *testing.T) {
	// A truncated stream must yield nothing, not a zero-padded frame.
	buf := bytes.NewReader(make([]byte, frameBytes-1))
	out := make(chan Frame, 1)
	if err := readFrames(buf, out); err != nil {
		t.Fatalf("readFrames: %v", err)
	}
	if len(out) != 0 {
		t.Fatal("partial frame must not be emitted")
	}
}

func TestReadFramesDropsWhenConsumerLags(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(make([]byte, frameBytes*3)) // 3 frames
	out := make(chan Frame, 1)            // room for one
	if err := readFrames(io.Reader(&buf), out); err != nil {
		t.Fatalf("readFrames: %v", err)
	}
	close(out)
	n := 0
	for range out {
		n++
	}
	if n > 1 {
		t.Fatalf("consumed = %d, want ≤1 (drops, not blocks)", n)
	}
}
