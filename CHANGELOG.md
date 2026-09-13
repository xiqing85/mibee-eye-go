# Changelog

Notable changes to MiBee Eye (Go implementation) are documented here.

## [0.2.0] — 2026-09-13

### Added

- **`camera.mode: v4l2`** — generic V4L2 capture for any Linux board
  (USB/UVC included): a pure-Go V4L2 layer (`internal/v4l2`, 64-bit UABI
  struct layouts pinned by size tests, explicit stubs on 32-bit) captures
  YU12 frames in-process and encodes them via the probed V4L2 M2M encoder
  (`camera.encoder_device`, default `/dev/video11`); when no capable M2M
  node exists it falls back to a resident ffmpeg subprocess.
- Release artifacts for linux amd64 / arm64 / armv7 (goreleaser, CGO off).

### Changed

- Repository renamed **mibee-eye-raspi-go → mibee-eye-go** (multi-board
  scope); binary and service names stay `mibee-eye` for drop-in upgrades.
  Old URLs redirect.
- Relicensed **CC BY-NC 4.0 → Apache-2.0** (NOTICE carries the
  MediaMTX-MIT third-party note).
- README hardware claims tightened: local capture modes
  (`mtxrpicam`/`rpicamvid`) are Pi-specific; other boards use `v4l2`
  (capture + optional ffmpeg fallback encoder) or `rtsp` (protocol
  gateway, any Linux host).

## [0.1.0] — 2026-09-13

Initial public release (shipped under CC BY-NC 4.0; see 0.2.0 for the
Apache-2.0 relicense).

### Capabilities

- ONVIF Profile S device: Device/Media/Imaging SOAP services + WS-Discovery
  (port 8080), powered by onvif-go/v2 — byte-stable element names for NVR
  raw-SOAP matching
- GB28181 device: SIP registration (UDP/TCP), digest auth, Catalog /
  DeviceInfo / RecordInfo queries, live / playback / download RTP-PS
  streaming, SIP INFO playback control (pause/resume/seek/speed), platform
  snapshot commands — powered by gb28181-go/device
- GB 35114 A-level (optional, `-tags gb35114`): SM2 certificate REGISTER
  auth + keyed-SM3 integrity
- RTSP server (port 8554, RTP over TCP interleaved or UDP) and RTMP push
- HLS live streaming for browsers: pure-Go MPEG-TS segmenter (no ffmpeg)
- Continuous local recording: H.264 segments + `index.jsonl`, retention and
  storage caps; GB28181 playback source
- Embedded SPEC v1 web admin UI (port 8088): cookie-session + CSRF auth,
  MSE/HLS live preview, partial-merge config API, SSE events, i18n (EN/中文)
- AI detection (optional, `-tags ai`): NanoDet-Plus ONNX on keyframes via
  passive AUHub subscription, fail-open capability gating
- Camera imaging controls (brightness/contrast/saturation/sharpness/WB/
  exposure) and flips baked in via a unified restart flow
- Snapshot via HTTP GET, runtime metrics API
