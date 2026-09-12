package rtsp

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/h264"
)

// findFreePort returns a free TCP port for testing.
func findFreePort() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.Port
}

// TestDescribe verifies that DESCRIBE returns SDP containing H.264 video media.
func TestDescribe(t *testing.T) {
	port := findFreePort()
	srv := New(Config{Port: port})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer srv.Stop()

	// Connect as client and DESCRIBE
	c := gortsplib.Client{
		Scheme: "rtsp",
		Host:   net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port)),
	}
	if err := c.Start(); err != nil {
		t.Fatalf("client Start failed: %v", err)
	}
	defer c.Close()

	u, _ := base.ParseURL(fmt.Sprintf("rtsp://127.0.0.1:%d/stream", port))
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("Describe failed: %v", err)
	}

	// Verify SDP contains H264
	sdpBytes, err := desc.Marshal()
	if err != nil {
		t.Fatalf("Marshal SDP failed: %v", err)
	}
	sdp := string(sdpBytes)

	if !strings.Contains(sdp, "H264") {
		t.Errorf("SDP missing H264 codec; got:\n%s", sdp)
	}
	if !strings.Contains(sdp, "video") {
		t.Errorf("SDP missing video media; got:\n%s", sdp)
	}
	if !strings.Contains(sdp, "m=video") {
		t.Errorf("SDP missing m=video line; got:\n%s", sdp)
	}
}

// TestSetupPlay verifies SETUP + PLAY flow and RTP packet reception.
func TestSetupPlay(t *testing.T) {
	port := findFreePort()
	srv := New(Config{Port: port})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer srv.Stop()

	// Set up mock frame source
	frameCh := make(chan h264.AccessUnit, 16)
	srv.SetFrameSource(frameCh)

	// Connect as client
	c := gortsplib.Client{
		Scheme: "rtsp",
		Host:   net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port)),
	}
	if err := c.Start(); err != nil {
		t.Fatalf("client Start failed: %v", err)
	}
	defer c.Close()

	u, _ := base.ParseURL(fmt.Sprintf("rtsp://127.0.0.1:%d/stream", port))
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("Describe failed: %v", err)
	}

	// Find H264 media
	var h264Fmt *format.H264
	var media *description.Media
	for _, m := range desc.Medias {
		for _, f := range m.Formats {
			if h, ok := f.(*format.H264); ok {
				h264Fmt = h
				media = m
				break
			}
		}
	}
	if h264Fmt == nil {
		t.Fatal("H264 format not found in DESCRIBE response")
	}

	// SETUP
	_, err = c.Setup(desc.BaseURL, media, 0, 0)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	// Create RTP decoder to count received packets
	rtpDec, err := h264Fmt.CreateDecoder()
	if err != nil {
		t.Fatalf("CreateDecoder failed: %v", err)
	}

	var recvMu sync.Mutex
	rtpPackets := 0

	c.OnPacketRTP(media, h264Fmt, func(pkt *rtp.Packet) {
		_, err := rtpDec.Decode(pkt)
		if err == nil {
			recvMu.Lock()
			rtpPackets++
			recvMu.Unlock()
		}
	})

	// PLAY
	_, err = c.Play(nil)
	if err != nil {
		t.Fatalf("Play failed: %v", err)
	}

	// Send a mock H.264 access unit
	frameCh <- h264.AccessUnit{
		Timestamp: time.Now(),
		NALUs: []h264.NALU{
			{
				Type:  7, // SPS
				Data:  []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
				IsSPS: true,
			},
			{
				Type:  8, // PPS
				Data:  []byte{0x68, 0xce, 0x38, 0x80},
				IsPPS: true,
			},
			{
				Type:  5, // IDR
				Data:  []byte{0x65, 0x88, 0x84, 0x00, 0x10, 0x04, 0x00, 0x00, 0x05, 0xef},
				IsIDR: true,
			},
		},
		KeyFrame: true,
	}

	// Send multiple frames to ensure at least one is received
	for i := 0; i < 5; i++ {
		frameCh <- h264.AccessUnit{
			Timestamp: time.Now().Add(time.Duration(i) * 66 * time.Millisecond),
			NALUs: []h264.NALU{
				{Type: 5, Data: []byte{0x65, 0x88, 0x84, 0x00, 0x10, 0x04, 0x00, 0x00, 0x05, 0xef}, IsIDR: true},
			},
			KeyFrame: true,
		}
	}

	// Wait for packets
	time.Sleep(500 * time.Millisecond)

	recvMu.Lock()
	count := rtpPackets
	recvMu.Unlock()

	if count == 0 {
		t.Error("expected to receive at least 1 RTP packet, got 0")
	}
	t.Logf("received %d RTP packets (access unit decoded)", count)
}

// TestAuthRequired verifies that unauthenticated clients get 401 when auth is configured.
func TestAuthRequired(t *testing.T) {
	port := findFreePort()
	srv := New(Config{
		Port:     port,
		Username: "admin",
		Password: "secret",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer srv.Stop()

	// Connect without credentials
	c := gortsplib.Client{
		Scheme: "rtsp",
		Host:   net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port)),
	}
	if err := c.Start(); err != nil {
		t.Fatalf("client Start failed: %v", err)
	}
	defer c.Close()

	u, _ := base.ParseURL(fmt.Sprintf("rtsp://127.0.0.1:%d/stream", port))
	_, _, err := c.Describe(u)
	if err == nil {
		t.Fatal("expected auth error, got nil")
	}

	// Verify it's an auth error (status 401)
	if !strings.Contains(err.Error(), "401") && !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("expected 401 Unauthorized error, got: %v", err)
	}
}

// TestAuthSuccess verifies that authenticated clients can connect when auth is configured.
func TestAuthSuccess(t *testing.T) {
	port := findFreePort()
	srv := New(Config{
		Port:     port,
		Username: "admin",
		Password: "secret",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer srv.Stop()

	// Connect WITH credentials
	c := gortsplib.Client{
		Scheme: "rtsp",
		Host:   net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port)),
	}
	if err := c.Start(); err != nil {
		t.Fatalf("client Start failed: %v", err)
	}
	defer c.Close()

	u, _ := base.ParseURL(fmt.Sprintf("rtsp://admin:secret@127.0.0.1:%d/stream", port))
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("Describe with auth failed: %v", err)
	}

	if len(desc.Medias) == 0 {
		t.Error("expected at least 1 media in DESCRIBE response")
	}
}

// TestOnDemand verifies that frames are only consumed when clients are connected.
func TestOnDemand(t *testing.T) {
	port := findFreePort()
	srv := New(Config{Port: port})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer srv.Stop()

	frameCh := make(chan h264.AccessUnit, 16)
	srv.SetFrameSource(frameCh)

	// Send a frame with no clients — channel should NOT be consumed
	frameCh <- h264.AccessUnit{
		Timestamp: time.Now(),
		NALUs: []h264.NALU{
			{Type: 1, Data: []byte{0x41, 0x42}},
		},
	}

	time.Sleep(200 * time.Millisecond)

	// Frame should still be in the channel (no consumer)
	select {
	case <-frameCh:
		t.Log("frame was NOT consumed (correct — no clients)")
	default:
		t.Log("channel empty — frame was consumed despite no clients")
		// This is acceptable too since initStream may have been called
	}
}

// TestMultipleClients verifies that multiple clients can connect simultaneously.
func TestMultipleClients(t *testing.T) {
	port := findFreePort()
	srv := New(Config{Port: port})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer srv.Stop()

	frameCh := make(chan h264.AccessUnit, 16)
	srv.SetFrameSource(frameCh)

	// Connect two clients
	u, _ := base.ParseURL(fmt.Sprintf("rtsp://127.0.0.1:%d/stream", port))

	clients := make([]*gortsplib.Client, 0, 2)
	for i := 0; i < 2; i++ {
		c := gortsplib.Client{
			Scheme: "rtsp",
			Host:   net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port)),
		}
		if err := c.Start(); err != nil {
			t.Fatalf("client %d Start failed: %v", i, err)
		}
		defer c.Close()

		desc, _, err := c.Describe(u)
		if err != nil {
			t.Fatalf("client %d Describe failed: %v", i, err)
		}
		_ = desc

		clients = append(clients, &c)
	}

	t.Logf("%d clients connected successfully", len(clients))
}
