# Contributing

Welcome! We're glad you want to contribute to the MiBee Eye Camera Service.

## Development Setup

- Go 1.26+ required
- Build: `make build` (or `go build -o build/mibee-eye ./cmd/server`)
- Test: `make test` (or `go test ./...`)
- Install dependencies: `go mod download`

## Code Style

- Use `gofmt -w` for formatting
- Run `go vet ./...` for static analysis
- Follow standard Go conventions: `log/slog` for logging, `fmt.Errorf("...: %w", err)` for error wrapping
- The default build is **zero CGO**; CGO appears only in optional build tags (`ai` links ONNX Runtime)

## Commit Convention

Use conventional commits:

- `feat(onvif): add WS-Discovery support`
- `fix(camera): handle device disconnect gracefully`
- `docs: update README`
- `ci: add golangci-lint`
- `test: add camera backend unit tests`

## Pull Request Process

1. Fork the repository
2. Create feature branch
3. Make commits following conventional format
4. Push to your fork
5. Submit pull request
6. Ensure CI checks pass (build / vet / tests / repo hygiene gate)
7. Address review feedback

## Project Structure

```
cmd/server/          # Main application binary
internal/
  camera/            # Camera capture (mtxrpicam pipe / rpicam-vid / RTSP source)
  onvif/             # ONVIF glue over onvif-go/v2
  gb35114auth/       # GB 35114 A-level auth (build tag gb35114)
  rtsp/              # RTSP server utilities
  rtmp/              # RTMP push
  hls/               # Pure-Go MPEG-TS segmenter
  h264/              # H.264 helpers
  recording/         # Continuous recording + index
  ai/                # NanoDet detection (build tag ai)
  metrics/           # Runtime metrics
  web/               # SPEC v1 web API + embedded UI
  config/            # Configuration management
  netutil/           # Network helpers
configs/             # config.example.yaml
deploy/              # systemd units
```

## Adding Camera Support

Capture modes live in `internal/camera/`: `camera.go` (mtxrpicam subprocess
pipe), `rpicamvid.go` (system rpicam-vid), and `rtsp_source.go` (pull from an
RTSP source). A new mode implements the internal `Camera` interface and is
selected via `camera.mode` in the YAML config.
