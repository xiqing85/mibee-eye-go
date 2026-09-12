package onvif

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sync"
	"testing"

	"github.com/mickeyzzc/gb28181-go/manscdp"
)

// snapUploadFixture returns a decodable JPEG via the SnapshotBuffer's
// ffmpeg transcode tier (same fixture as snapshot_test.go); skips when
// ffmpeg or the fixture is unavailable, or when rpicam-still would win.
func snapUploadFixture(t *testing.T) *SnapshotBuffer {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	if _, err := exec.LookPath("rpicam-still"); err == nil {
		t.Skip("rpicam-still present: tier 1 would win")
	}
	sb := NewSnapshotBuffer(true)
	sb.Update(loadSnapshotFixture(t))
	if !sb.HasFrame() {
		t.Fatal("fixture produced no frame")
	}
	return sb
}

func TestSnapshotUploaderPostsEachFrameAndReturnsPaths(t *testing.T) {
	sb := snapUploadFixture(t)

	var mu sync.Mutex
	var gotURLs []string
	var gotBodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotURLs = append(gotURLs, r.URL.String())
		gotBodies = append(gotBodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "stored",
			"path":   "store/2026/09/10/frame.jpg",
		})
	}))
	defer srv.Close()

	up := &SnapshotUploader{SB: sb, Client: srv.Client()}
	ids, err := up.Execute(context.Background(), manscdp.SnapShotCmd{
		SnapNum:   2,
		UploadURL: srv.URL + "/api/gb28181/snapshot/upload?session=0123456789abcdef0123456789abcdef",
		SessionID: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(ids) != 2 || ids[0] != "store/2026/09/10/frame.jpg" {
		t.Fatalf("ids = %v", ids)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotURLs) != 2 {
		t.Fatalf("uploads = %d, want 2", len(gotURLs))
	}
	// The URL is used verbatim — session parameter included.
	wantURL := "/api/gb28181/snapshot/upload?session=0123456789abcdef0123456789abcdef"
	for _, u := range gotURLs {
		if u != wantURL {
			t.Fatalf("upload URL = %q, want %q", u, wantURL)
		}
	}
	for i, b := range gotBodies {
		if len(b) < 100 || b[0] != 0xFF || b[1] != 0xD8 {
			t.Fatalf("upload %d is not a JPEG (len=%d)", i+1, len(b))
		}
	}
}

func TestSnapshotUploaderPartialFailureReturnsSurvivingIDs(t *testing.T) {
	sb := snapUploadFixture(t)

	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n > 1 {
			http.Error(w, "session finished", http.StatusConflict)
			return
		}
		_, _ = w.Write([]byte(`{"status":"stored","path":"store/a.jpg"}`))
	}))
	defer srv.Close()

	up := &SnapshotUploader{SB: sb, Client: srv.Client()}
	ids, err := up.Execute(context.Background(), manscdp.SnapShotCmd{
		SnapNum:   2,
		UploadURL: srv.URL + "/upload",
		SessionID: "s",
	})
	if err != nil {
		t.Fatalf("Execute: partial failure should not fail the exchange: %v", err)
	}
	if len(ids) != 1 || ids[0] != "store/a.jpg" {
		t.Fatalf("ids = %v, want the single survivor", ids)
	}
}

func TestSnapshotUploaderAllFailuresReturnError(t *testing.T) {
	sb := snapUploadFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unknown session", http.StatusNotFound)
	}))
	defer srv.Close()

	up := &SnapshotUploader{SB: sb, Client: srv.Client()}
	ids, err := up.Execute(context.Background(), manscdp.SnapShotCmd{
		SnapNum:   1,
		UploadURL: srv.URL + "/upload",
		SessionID: "s",
	})
	if err == nil {
		t.Fatalf("want error when every frame fails, ids=%v", ids)
	}
	if ids != nil {
		t.Fatalf("ids = %v, want nil", ids)
	}
}

func TestSnapshotUploaderWithoutFramesFailsFast(t *testing.T) {
	if _, err := exec.LookPath("rpicam-still"); err == nil {
		t.Skip("rpicam-still present: tier 1 may capture a real frame")
	}
	sb := NewSnapshotBuffer(true) // never updated — no frame
	up := &SnapshotUploader{SB: sb}
	ids, err := up.Execute(context.Background(), manscdp.SnapShotCmd{SnapNum: 1, UploadURL: "http://x/u"})
	if err == nil || ids != nil {
		t.Fatalf("want error, got ids=%v err=%v", ids, err)
	}
}
