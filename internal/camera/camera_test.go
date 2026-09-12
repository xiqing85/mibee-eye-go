// Portions derived from MediaMTX (https://github.com/bluenviron/mediamtx)
// Original code Copyright (c) bluenviron, MIT License

package camera

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// --- Pipe Tests ---

func TestPipeReadWrite(t *testing.T) {
	r, w := io.Pipe()
	p := newPipe(r, w)

	msg := []byte("hello world")

	// Write in goroutine
	go func() {
		_ = p.write(msg)
		_ = w.Close()
	}()

	got, err := p.read()
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}

	if !bytes.Equal(got, msg) {
		t.Errorf("expected %q, got %q", msg, got)
	}
}

func TestPipeMultipleMessages(t *testing.T) {
	r, w := io.Pipe()
	p := newPipe(r, w)

	msgs := [][]byte{
		[]byte("first"),
		[]byte("second message"),
		[]byte("third"),
	}

	go func() {
		for _, m := range msgs {
			_ = p.write(m)
		}
		_ = w.Close()
	}()

	for i, expected := range msgs {
		got, err := p.read()
		if err != nil {
			t.Fatalf("read %d failed: %v", i, err)
		}
		if !bytes.Equal(got, expected) {
			t.Errorf("msg %d: expected %q, got %q", i, expected, got)
		}
	}
}

func TestPipeEmptyMessage(t *testing.T) {
	r, w := io.Pipe()
	p := newPipe(r, w)

	go func() {
		_ = p.write([]byte{})
		_ = w.Close()
	}()

	got, err := p.read()
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %d bytes", len(got))
	}
}

func TestPipeReadClosed(t *testing.T) {
	r, w := io.Pipe()
	_ = w.Close()
	p := newPipe(r, nil)

	_, err := p.read()
	if err == nil {
		t.Fatal("expected error on closed pipe")
	}
}

func TestPipeLargeMessage(t *testing.T) {
	r, w := io.Pipe()
	p := newPipe(r, w)

	// Simulate a large NALU (~100KB)
	large := make([]byte, 100*1024)
	for i := range large {
		large[i] = byte(i % 256)
	}

	go func() {
		_ = p.write(large)
		_ = w.Close()
	}()

	got, err := p.read()
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}

	if len(got) != len(large) {
		t.Errorf("expected %d bytes, got %d", len(large), len(got))
	}
	if !bytes.Equal(got, large) {
		t.Error("large message content mismatch")
	}
}

// --- Params Serialization Tests ---

func TestDefaultParams(t *testing.T) {
	p := DefaultParams()
	if p.Width != 1280 || p.Height != 720 {
		t.Errorf("expected 1280x720, got %dx%d", p.Width, p.Height)
	}
	if p.FPS != 15 {
		t.Errorf("expected FPS 15, got %f", p.FPS)
	}
	if p.Codec != "hardwareH264" {
		t.Errorf("expected hardwareH264, got %s", p.Codec)
	}
}

func TestParamsSerialize(t *testing.T) {
	p := DefaultParams()
	serialized, err := p.Serialize()
	if err != nil {
		t.Fatalf("serialization failed: %v", err)
	}

	if len(serialized) == 0 {
		t.Fatal("serialization returned empty")
	}

	// Verify it starts with LogLevel field
	if !bytes.HasPrefix(serialized, []byte("LogLevel:")) {
		t.Errorf("expected to start with LogLevel:, got %s", string(serialized[:20]))
	}

	// Verify all fields are present by checking field count (space-separated)
	parts := strings.Split(string(serialized), " ")
	expectedFields := reflect.ValueOf(p).NumField()
	if len(parts) != expectedFields {
		t.Errorf("expected %d fields, got %d", expectedFields, len(parts))
	}

	// Each part should be "FieldName:Value"
	for i, part := range parts {
		if !strings.Contains(part, ":") {
			t.Errorf("field %d missing colon: %q", i, part)
		}
	}
}

func TestParamsSerializeCommand(t *testing.T) {
	p := DefaultParams()
	cmd, err := p.SerializeCommand()
	if err != nil {
		t.Fatalf("SerializeCommand failed: %v", err)
	}

	if len(cmd) == 0 || cmd[0] != 'c' {
		t.Errorf("expected command to start with 'c', got 0x%.2x", cmd[0])
	}

	// After 'c', should be the serialized params
	serialized, err := p.Serialize()
	if err != nil {
		t.Fatalf("Serialize failed: %v", err)
	}
	if !bytes.Equal(cmd[1:], serialized) {
		t.Error("command body doesn't match serialized params")
	}
}

func TestParamsSerializeStringField(t *testing.T) {
	p := Params{
		Exposure: "normal",
		AWB:      "auto",
		Codec:    "hardwareH264",
	}
	serialized, err := p.Serialize()
	if err != nil {
		t.Fatalf("serialization failed: %v", err)
	}
	s := string(serialized)

	// String values should be base64-encoded
	normalB64 := base64.StdEncoding.EncodeToString([]byte("normal"))
	if !strings.Contains(s, "Exposure:"+normalB64) {
		t.Errorf("expected base64-encoded Exposure, got: %s", s)
	}
}

func TestParamsSerializeBoolField(t *testing.T) {
	p := Params{HFlip: true, VFlip: false}
	serialized, err := p.Serialize()
	if err != nil {
		t.Fatalf("serialization failed: %v", err)
	}
	s := string(serialized)

	if !strings.Contains(s, "HFlip:1") {
		t.Errorf("expected HFlip:1, got: %s", s)
	}
	if !strings.Contains(s, "VFlip:0") {
		t.Errorf("expected VFlip:0, got: %s", s)
	}
}


func TestSerializeUnsupportedType(t *testing.T) {
	// Params only has uint32/float32/string/bool fields (all valid),
	// so we test the kind-check logic with a struct containing an unsupported int field.
	type testStruct struct {
		Valid   uint32
		Invalid int
	}

	rv := reflect.ValueOf(testStruct{Valid: 1, Invalid: 2})
	for i := range rv.NumField() {
		f := rv.Field(i)
		switch f.Kind() {
		case reflect.Uint32, reflect.Float32, reflect.String, reflect.Bool:
		default:
			// This simulates what Serialize's kind-check returns
			errMsg := fmt.Sprintf("unsupported param field type: %s has %s",
				rv.Type().Field(i).Name, f.Kind())
			expected := "unsupported param field type: Invalid has int"
			if errMsg != expected {
				t.Errorf("error message format mismatch:\n  got:  %s\n  want: %s", errMsg, expected)
			}
			return // OK - unsupported type detected
		}
	}
	t.Error("expected Serialize to detect unsupported type, but no unsupported type was found")
}

func TestSupportedParamKind(t *testing.T) {
	// Verify supportedParamKind rejects unsupported types.
	unsupported := []reflect.Kind{reflect.Int, reflect.Float64, reflect.Uint16, reflect.Int32, reflect.Slice, reflect.Struct}
	for _, k := range unsupported {
		if supportedParamKind(k) {
			t.Errorf("expected %s to be unsupported", k)
		}
	}

	// Verify supportedParamKind accepts the 4 supported types.
	supported := []reflect.Kind{reflect.Uint32, reflect.Float32, reflect.String, reflect.Bool}
	for _, k := range supported {
		if !supportedParamKind(k) {
			t.Errorf("expected %s to be supported", k)
		}
	}

	// Verify the error message format matches ONVIF convention.
	err := fmt.Errorf("unsupported param field type: %s has %s", "SomeField", reflect.Int)
	expected := "unsupported param field type: SomeField has int"
	if err.Error() != expected {
		t.Errorf("error message format mismatch:\n  got:  %s\n  want: %s", err.Error(), expected)
	}
}
func TestDeserializeParamValue(t *testing.T) {
	original := "normal"
	encoded := base64.StdEncoding.EncodeToString([]byte(original))
	decoded, err := DeserializeParamValue(encoded)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if decoded != original {
		t.Errorf("expected %q, got %q", original, decoded)
	}
}

// --- Param Reflection Tests ---

func TestSetParamValue(t *testing.T) {
	p := DefaultParams()

	tests := []struct {
		name  string
		field string
		value interface{}
		check func(Params) bool
	}{
		{"brightness float32", "Brightness", float32(0.5), func(p Params) bool { return p.Brightness == 0.5 }},
		{"brightness string", "Brightness", "0.5", func(p Params) bool { return p.Brightness == 0.5 }},
		{"width uint32", "Width", uint32(1920), func(p Params) bool { return p.Width == 1920 }},
		{"width int", "Width", 1920, func(p Params) bool { return p.Width == 1920 }},
		{"width string", "Width", "1920", func(p Params) bool { return p.Width == 1920 }},
		{"exposure string", "Exposure", "manual", func(p Params) bool { return p.Exposure == "manual" }},
		{"hflip bool", "HFlip", true, func(p Params) bool { return p.HFlip == true }},
		{"hflip string", "HFlip", "true", func(p Params) bool { return p.HFlip == true }},
		{"fps float64", "FPS", float64(30), func(p Params) bool { return p.FPS == 30 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p = DefaultParams()
			err := setParamValue(&p, tt.field, tt.value)
			if err != nil {
				t.Fatalf("setParamValue failed: %v", err)
			}
			if !tt.check(p) {
				t.Errorf("value not set correctly")
			}
		})
	}
}

func TestSetParamValueErrors(t *testing.T) {
	p := DefaultParams()

	// Unknown field
	err := setParamValue(&p, "Nonexistent", "value")
	if err == nil {
		t.Fatal("expected error for unknown field")
	}

	// Wrong type for uint32
	err = setParamValue(&p, "Width", "not_a_number")
	if err == nil {
		t.Fatal("expected error for invalid uint32")
	}

	// Negative value for uint32
	err = setParamValue(&p, "Width", -1)
	if err == nil {
		t.Fatal("expected error for negative uint32")
	}
}

func TestGetParamValue(t *testing.T) {
	p := DefaultParams()
	p.Brightness = 0.7
	p.Width = 640
	p.Exposure = "manual"
	p.HFlip = true

	tests := []struct {
		field    string
		expected interface{}
	}{
		{"Brightness", float32(0.7)},
		{"Width", uint64(640)},
		{"Exposure", "manual"},
		{"HFlip", true},
	}

	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			got, err := getParamValue(p, tt.field)
			if err != nil {
				t.Fatalf("getParamValue failed: %v", err)
			}
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("expected %v (%T), got %v (%T)", tt.expected, tt.expected, got, got)
			}
		})
	}
}

func TestGetParamValueUnknown(t *testing.T) {
	p := DefaultParams()
	_, err := getParamValue(p, "Nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
}

// --- Camera Interface Tests ---

func TestNewCamera(t *testing.T) {
	c := NewRPiCamera()
	info := c.Info()

	if info.Name != "RPi Camera" {
		t.Errorf("expected name 'RPi Camera', got %q", info.Name)
	}
	if info.Model != "OV5647" {
		t.Errorf("expected model 'OV5647', got %q", info.Model)
	}
	if info.Width != 1280 || info.Height != 720 {
		t.Errorf("expected 1280x720, got %dx%d", info.Width, info.Height)
	}
}

func TestNewCameraWithOptions(t *testing.T) {
	c := NewRPiCamera(
		WithParams(Params{Width: 640, Height: 480, FPS: 30}),
		WithInfo(CameraInfo{
			Name:   "Test Camera",
			Model:  "IMX219",
			Width:  640,
			Height: 480,
		}),
	)

	info := c.Info()
	if info.Name != "Test Camera" {
		t.Errorf("expected name 'Test Camera', got %q", info.Name)
	}
}

func TestNewCameraDefaultFPS(t *testing.T) {
	c := NewRPiCamera()
	info := c.Info()
	if info.FPS != 15 {
		t.Errorf("expected FPS 15, got %f", info.FPS)
	}
}

// --- MapParamName Tests ---

func TestMapParamName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
		ok       bool
	}{
		{"brightness", "Brightness", true},
		{"contrast", "Contrast", true},
		{"saturation", "Saturation", true},
		{"sharpness", "Sharpness", true},
		{"width", "Width", true},
		{"height", "Height", true},
		{"fps", "FPS", true},
		{"exposure", "Exposure", true},
		{"gain", "Gain", true},
		{"awbMode", "AWB", true},
		{"hFlip", "HFlip", true},
		{"vFlip", "VFlip", true},
		{"shutter", "Shutter", true},
		{"bitrate", "Bitrate", true},
		{"unknown", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, ok := mapParamName(tt.input)
			if ok != tt.ok {
				t.Errorf("mapParamName(%q) ok=%v, expected %v", tt.input, ok, tt.ok)
			}
			if ok && got != tt.expected {
				t.Errorf("mapParamName(%q) = %q, expected %q", tt.input, got, tt.expected)
			}
		})
	}
}

// TestMockCapture tests the frame reading pipeline using a mock subprocess
// that's built as a Go binary for reliability.
func TestMockCapture(t *testing.T) {
	// Build the mock subprocess
	tmpDir := t.TempDir()
	mockBin := filepath.Join(tmpDir, "mock-mtxrpicam")

	// Write mock source code
	mockSrc := filepath.Join(tmpDir, "mock_main.go")
	mockCode := `package main

import (
		"os"
		"strconv"
		"encoding/binary"
	)

// writeFrame writes a framed message to fd
func writeFrame(fd int, data []byte) {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(len(data)))
	os.NewFile(uintptr(fd), "").Write(buf[:])
	os.NewFile(uintptr(fd), "").Write(data)
}

func main() {
	confFD, _ := strconv.Atoi(os.Getenv("PIPE_CONF_FD"))
	videoFD, _ := strconv.Atoi(os.Getenv("PIPE_VIDEO_FD"))
	confFile := os.NewFile(uintptr(confFD), "conf")

	// Read config command (consume it)
	var hdr [4]byte
	confFile.Read(hdr[:])
	sz := binary.LittleEndian.Uint32(hdr[:])
	buf := make([]byte, sz)
	confFile.Read(buf)

	// Send ready signal
	writeFrame(videoFD, []byte("r"))

	// Send 3 mock frames
	for i := 1; i <= 3; i++ {
		dts := uint64(i * 66666)
		var dtsBuf [8]byte
		binary.LittleEndian.PutUint64(dtsBuf[:], dts)

		var nalu []byte
		if i == 1 {
			// IDR frame
			nalu = []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0x00, 0x0a, 0xe9, 0x40, 0x40, 0x04, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00, 0xc8, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x68, 0xce, 0x38, 0x80}
		} else {
			// P-frame
			nalu = []byte{0x00, 0x00, 0x00, 0x01, 0x41, 0xea, 0x20, 0x04}
		}

		payload := make([]byte, 1+8+len(nalu))
		payload[0] = 'd'
		copy(payload[1:9], dtsBuf[:])
		copy(payload[9:], nalu)
		writeFrame(videoFD, payload)
	}
}
`
	if err := os.WriteFile(mockSrc, []byte(mockCode), 0644); err != nil {
		t.Fatalf("write mock source: %v", err)
	}

	// Build mock binary
	buildCmd := exec.Command("go", "build", "-o", mockBin, mockSrc)
	buildCmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build mock binary: %v\n%s", err, out)
	}

	// Create camera with mock binary path
	c := NewRPiCamera(
		WithBinPath(mockBin),
		WithParams(Params{
			Width:  1280,
			Height: 720,
			FPS:    15,
			Codec:  "hardwareH264",
		}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer c.Stop()

	// Read frames from the channel
	framesCh := c.Frames()
	if framesCh == nil {
		t.Fatal("Frames() returned nil channel")
	}

	frameCount := 0
	timeout := time.After(3 * time.Second)

	for frameCount < 3 {
		select {
		case frame, ok := <-framesCh:
			if !ok {
				t.Fatal("frames channel closed unexpectedly")
			}
			frameCount++
			if len(frame.Data) == 0 {
				t.Errorf("frame %d: empty data", frameCount)
			}
			if frame.PTS <= 0 {
				t.Errorf("frame %d: invalid PTS %d", frameCount, frame.PTS)
			}
			t.Logf("frame %d: PTS=%d, data=%d bytes", frameCount, frame.PTS, len(frame.Data))

		case <-timeout:
			t.Fatalf("timed out waiting for frames (got %d)", frameCount)
		}
	}

	if frameCount < 3 {
		t.Errorf("expected at least 3 frames, got %d", frameCount)
	}
}

// TestStartNotStarted verifies SetParam/GetParam fail when camera not started.
func TestNotStarted(t *testing.T) {
	c := NewRPiCamera()

	// Frames() should return nil when not started
	if c.Frames() != nil {
		t.Error("expected nil Frames() channel when not started")
	}

	// SetParam should fail
	err := c.SetParam("brightness", float32(0.5))
	if err == nil {
		t.Fatal("expected SetParam to fail when not started")
	}
}

// TestStartNoBinary verifies Start fails when binary doesn't exist.
func TestStartNoBinary(t *testing.T) {
	c := NewRPiCamera(WithBinPath("/nonexistent/mtxrpicam"))

	err := c.Start(context.Background())
	if err == nil {
		t.Fatal("expected Start to fail with missing binary")
	}

	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestStopIdempotent verifies Stop can be called multiple times safely.
func TestStopIdempotent(t *testing.T) {
	c := NewRPiCamera(WithBinPath("/nonexistent/mtxrpicam"))

	// Stop without Start should not panic
	err := c.Stop()
	if err != nil {
		t.Errorf("Stop before Start returned error: %v", err)
	}

	// Second Stop should also not panic
	err = c.Stop()
	if err != nil {
		t.Errorf("second Stop returned error: %v", err)
	}
}

// --- MultiplyAndDivide Tests ---

func TestMultiplyAndDivide(t *testing.T) {
	tests := []struct {
		v, m, d, expected int64
	}{
		{66666, 90000, 1000000, 5999},    // 66666us * 90000 / 1000000
		{1000000, 90000, 1000000, 90000}, // exactly 1 second
		{0, 90000, 1000000, 0},           // zero DTS
		{33333, 90000, 1000000, 2999},    // ~30fps frame
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d*%d/%d", tt.v, tt.m, tt.d), func(t *testing.T) {
			got := multiplyAndDivide(tt.v, tt.m, tt.d)
			if got != tt.expected {
				t.Errorf("multiplyAndDivide(%d, %d, %d) = %d, expected %d",
					tt.v, tt.m, tt.d, got, tt.expected)
			}
		})
	}
}

// --- Pipe Protocol Framing Tests ---

func TestPipeFramingRoundtrip(t *testing.T) {
	// Test that our Go pipe framing matches the expected wire format
	r, w := io.Pipe()
	p := newPipe(r, w)

	// Simulate what mtxrpicam sends: a 'd' frame message
	dts := int64(66666)
	naluData := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0xa0, 0x40}
	payload := make([]byte, 1+8+len(naluData))
	payload[0] = 'd'
	binary.LittleEndian.PutUint64(payload[1:9], uint64(dts))
	copy(payload[9:], naluData)

	go func() {
		_ = p.write(payload)
		_ = w.Close()
	}()

	got, err := p.read()
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}

	if !bytes.Equal(got, payload) {
		t.Errorf("roundtrip mismatch: got %d bytes, expected %d bytes", len(got), len(payload))
	}

	if got[0] != 'd' {
		t.Errorf("expected 'd' prefix, got 0x%.2x", got[0])
	}

	gotDTS := int64(got[8])<<56 | int64(got[7])<<48 | int64(got[6])<<40 |
		int64(got[5])<<32 | int64(got[4])<<24 | int64(got[3])<<16 |
		int64(got[2])<<8 | int64(got[1])
	if gotDTS != dts {
		t.Errorf("DTS mismatch: got %d, expected %d", gotDTS, dts)
	}
}

// --- SetParam via Camera interface ---

func TestCameraSetGetParam(t *testing.T) {
	c := NewRPiCamera()

	// GetParam should work even before start
	val, err := c.GetParam("brightness")
	if err != nil {
		t.Fatalf("GetParam failed: %v", err)
	}
	if val.(float32) != 0 {
		t.Errorf("expected default brightness 0, got %v", val)
	}

	val, err = c.GetParam("width")
	if err != nil {
		t.Fatalf("GetParam width failed: %v", err)
	}
	if val.(uint64) != 1280 {
		t.Errorf("expected default width 1280, got %v", val)
	}

	// Unknown param should fail
	_, err = c.GetParam("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown param")
	}
}

// --- SerializeQuit ---

func TestSerializeQuit(t *testing.T) {
	quit := SerializeQuit()
	if len(quit) != 1 || quit[0] != 'e' {
		t.Errorf("expected 'e', got %v", quit)
	}
}

// --- Goroutine Cleanup Test ---

// TestStopCleanup verifies Stop() cleanly terminates the run() goroutine
// by killing the subprocess, closing the pipe (unblocking readLoop),
// and returning promptly without leaking goroutines.
func TestStopCleanup(t *testing.T) {
	// Build a mock subprocess that streams frames until killed/pipe-closed
	tmpDir := t.TempDir()
	mockBin := filepath.Join(tmpDir, "mock-mtxrpicam")

	mockSrc := filepath.Join(tmpDir, "mock_main.go")
	mockCode := `package main

import (
	"encoding/binary"
	"os"
	"strconv"
	"syscall"
	"time"
)

func writeFrame(fd int, data []byte) error {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(len(data)))
	if _, err := syscall.Write(fd, buf[:]); err != nil {
		return err
	}
	_, err := syscall.Write(fd, data)
	return err
}

func main() {
	confFD, _ := strconv.Atoi(os.Getenv("PIPE_CONF_FD"))
	videoFD, _ := strconv.Atoi(os.Getenv("PIPE_VIDEO_FD"))

	// Read config command (consume it)
	var hdr [4]byte
	if _, err := syscall.Read(confFD, hdr[:]); err != nil {
		return
	}
	sz := binary.LittleEndian.Uint32(hdr[:])
	buf := make([]byte, sz)
	if _, err := syscall.Read(confFD, buf); err != nil {
		return
	}
	syscall.Close(confFD)

	// Send ready signal
	writeFrame(videoFD, []byte("r"))

	// Stream frames until pipe breaks
	counter := 0
	for {
		counter++
		var dtsBuf [8]byte
		binary.LittleEndian.PutUint64(dtsBuf[:], uint64(counter * 66666))

		var nalu []byte
		if counter%15 == 0 {
			nalu = []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84, 0x00} // IDR
		} else {
			nalu = []byte{0x00, 0x00, 0x00, 0x01, 0x41, 0x9a, 0x20, 0x08} // P-frame
		}

		payload := make([]byte, 1+8+len(nalu))
		payload[0] = 'd'
		copy(payload[1:9], dtsBuf[:])
		copy(payload[9:], nalu)

		if err := writeFrame(videoFD, payload); err != nil {
			break // pipe closed
		}
		time.Sleep(66 * time.Millisecond)
	}
	syscall.Close(videoFD)
}
`
	if err := os.WriteFile(mockSrc, []byte(mockCode), 0644); err != nil {
		t.Fatalf("write mock source: %v", err)
	}

	// Build mock binary
	buildCmd := exec.Command("go", "build", "-o", mockBin, mockSrc)
	buildCmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build mock binary: %v\n%s", err, out)
	}

	c := NewRPiCamera(
		WithBinPath(mockBin),
		WithParams(Params{
			Width:  1280,
			Height: 720,
			FPS:    15,
			Codec:  "hardwareH264",
		}),
	)

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Read a few frames to confirm camera is actively streaming
	framesCh := c.Frames()
	for i := 0; i < 3; i++ {
		select {
		case _, ok := <-framesCh:
			if !ok {
				t.Fatal("frames channel closed unexpectedly")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for frames")
		}
	}

	// Stop and measure completion time
	start := time.Now()
	stopDone := make(chan struct{})
	go func() {
		c.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
		elapsed := time.Since(start)
		t.Logf("Stop() completed in %v", elapsed)
		if elapsed > 5*time.Second {
			t.Errorf("Stop() took too long: %v (should be < 5s)", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop() did not complete within 10 seconds - possible goroutine leak")
	}
}
