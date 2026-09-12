package onvif

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mickeyzzc/gb28181-go/manscdp"
)

// SnapshotUploader executes GB/T 28181-2022 snapshot commands
// (DeviceControl/SnapShot, A.2.1.24) for the gb28181-go device server:
// it captures cmd.SnapNum JPEG frames from the SnapshotBuffer (the same
// tiers the /snapshot endpoint serves) and POSTs each body to the
// command's UploadURL **verbatim** — the URL already carries the session
// parameter; the receiving platform owns that contract. The JSON
// response's "path" becomes the uploaded-file ID echoed in the
// UploadSnapShotFinished notify.
type SnapshotUploader struct {
	// SB supplies the JPEG frames.
	SB *SnapshotBuffer
	// Client is optional; a 5s-timeout default is used when nil.
	Client *http.Client
}

// snapshotUploadMaxResponse bounds the platform's JSON reply.
const snapshotUploadMaxResponse = 4 * 1024

// Execute implements gbdev.SnapshotExecutor. Frames with capture or
// upload failures are skipped; the returned slice carries one ID per
// uploaded frame (a partial result — the platform derives
// complete/partial/failed from the count vs SnapNum).
func (u *SnapshotUploader) Execute(ctx context.Context, cmd manscdp.SnapShotCmd) ([]string, error) {
	n := cmd.SnapNum
	if n < 1 {
		n = 1
	}
	if n > 10 { // spec: SnapNum 1..10
		n = 10
	}
	interval := time.Duration(cmd.Interval) * time.Second
	if interval < time.Second {
		interval = time.Second // platform session managers expect >=1s cadence
	}
	client := u.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}

	ids := make([]string, 0, n)
	var lastErr error
	for i := 0; i < n; i++ {
		if i > 0 {
			select {
			case <-time.After(interval):
			case <-ctx.Done():
				return ids, ctx.Err()
			}
		}
		jpeg, contentType, err := u.SB.Snapshot()
		if err != nil {
			lastErr = fmt.Errorf("capture frame %d: %w", i+1, err)
			continue
		}
		if contentType != "image/jpeg" {
			lastErr = fmt.Errorf("capture frame %d: snapshot tier produced %s, not JPEG", i+1, contentType)
			continue
		}
		id, err := u.postFrame(ctx, client, cmd.UploadURL, jpeg)
		if err != nil {
			lastErr = fmt.Errorf("upload frame %d: %w", i+1, err)
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return ids, nil
}

// postFrame POSTs one raw JPEG body and returns the platform's file ID.
func (u *SnapshotUploader) postFrame(ctx context.Context, client *http.Client, url string, jpeg []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jpeg))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "image/jpeg")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("platform answered %s", resp.Status)
	}

	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, snapshotUploadMaxResponse)).Decode(&body); err != nil {
		return "", fmt.Errorf("decode reply: %w", err)
	}
	if body.Path == "" {
		return "", fmt.Errorf("platform reply carries no path")
	}
	return body.Path, nil
}
