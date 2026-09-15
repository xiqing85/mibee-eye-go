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

## No Hardcoded Values

Deployment-relevant values — device paths, addresses, external binary
names, device IDs, version strings — must come from configuration; the
single place a default may be declared is the config module
(`src/config/` in Rust, `internal/config/` in Go). Business code reads
config and never invents defaults. Tests are exempt (golden semantics).
`tools/check-hardcode.sh` gates this in CI; a genuinely semantic
constant gets a same-line `hardcode-ok: <reason>` annotation, justified
in the commit message.

## Release Cadence

Releases follow a split rule across the two MiBee Eye implementations
([mibee-eye-rs](https://github.com/xiqing85/mibee-eye-rs) and
[mibee-eye-go](https://github.com/xiqing85/mibee-eye-go)):

- **Minor releases (x.y.0, capability packages) are synchronized**: same
  version number, same day, cross-linked bilingual release notes. A
  capability ships only when both implementations are ready; if one lags,
  the release waits. New features never ship in a patch.
- **Patch releases (x.y.z > 0, fixes only) are independent**: either repo
  may publish its own patch with its own number (mibee-eye-go can be at
  0.2.1 while mibee-eye-rs is at 0.2.3) — no lockstep for bug, security
  or regression fixes.

See [docs/roadmap-v0.3.0.md](docs/roadmap-v0.3.0.md) for the current
synchronized capability package.

## Project Structure

```
cmd/server/          # Main application binary
internal/
  camera/            # Camera capture (mtxrpicam pipe / rpicam-vid / in-process V4L2 / RTSP source)
  v4l2/              # Pure-Go V4L2 layer (capture, M2M encode, probe; 64-bit UABI size-pinned)
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
pipe), `rpicamvid.go` (system rpicam-vid), `v4l2_source.go` (in-process
generic V4L2 capture — any board, USB/UVC included, M2M hardware encode with
an ffmpeg fallback), and `rtsp_source.go` (pull from an RTSP source). A new
mode implements the internal `Camera` interface and is selected via
`camera.mode` in the YAML config.
