# Changelog

Notable changes to MiBee Eye (Go implementation) are documented here.

> **Release cadence** — minor versions (x.y.0, capability packages) are
> synchronized with the Rust implementation
> ([mibee-eye-rs](https://github.com/xiqing85/mibee-eye-rs)): same
> version number, same day, cross-linked notes. Patch versions (fixes
> only) are independent — either repo may publish its own patch number.
> The next synchronized minor is v0.3.0 (headline scope: complete
> GB/T 28181-2022 device-role coverage — see
> [docs/roadmap-v0.3.0.md](docs/roadmap-v0.3.0.md)).

## [Unreleased]

- **Device-level rotation baked into the stream** (SPEC v1 appendix A
  #19): new `camera.rotation` key (0 | 90 | 180 | 270, clockwise
  degrees) rotates the stream for every consumer — RTSP, ONVIF, GB28181,
  recordings, snapshots and AI detection all see it, with 90/270
  swapping the announced resolution (ONVIF Profile S, `/api/status`,
  MSE, AI bbox space). Mode support: `v4l2` transposes the raw YU12
  frames in-process (full quarter-turn range); `rpicamvid` accepts
  **0/180 only** — Raspberry Pi libcamera (vc4/PiSP) rejects transpose
  transforms (verified live: Pi 3B / IMX219 / libcamera 0.7.1 →
  "transforms requiring transpose not supported", rpicam-vid crash
  loop), so 90/270 are rejected at validation with a pointer to the
  v4l2 mode; `mtxrpicam`/`rtsp` reject non-zero values at validation.
  Validation also requires quarter turns and even capture
  dimensions for 90/270. Tier-1 rpicam-still snapshots now apply the
  stream's transform flags (`--rotation/--hflip/--vflip`) so JPEGs match
  the video orientation — flips previously missed there too. Bonus: the
  v4l2 backend now actually applies the device flips (they were silently
  ignored before), including runtime changes (web imaging / GB
  FrameMirror) via transform atomics. Previously `camera.rotation` was a
  display-only CSS convention on the web UI — retired in the same
  frontend (webui PR #11).
- **PUT /api/config validates before persisting** (found live during the
  rotation verification): the deep-merge path wrote the YAML and
  answered 200 without validating — an invalid value (e.g.
  `camera.rotation: 45`, or a zero fps) survived to the next boot, where
  config validation exits the process → systemd restart loop, device
  down. The merged document is now parsed + validated before the atomic
  write; invalid input answers 400 and leaves the file (and the running
  service) untouched.
- **Device serial fallback** (issue #39): an empty `device.serial_number`
  no longer reaches `GetDeviceInformation` — after config/env, the boot
  probes a device-level, interface-independent identity (Raspberry Pi
  `/proc/cpuinfo` `Serial`, else the Linux machine-id, both documented
  locations) and logs the effective value. MACs are deliberately NOT
  used (dual-homed boards would flip identity); probe failure keeps the
  configured value with a warning. Unblocks the NVR's stable_id dedup
  and cross-subnet rediscovery.
- **ONVIF Pull-Point events service** (onvif-go v2.2): AI motion alarms
  now also publish as `tns1:VideoSource/MotionAlarm` while an NVR holds
  a pull-point subscription — the same accepted rising edge (edge +
  AlarmReport gate + cooldown) that feeds the GB alarm NOTIFY and the
  SPEC v1 §6 `alarm` SSE event. New config key `onvif.events_enabled`
  (default `true`); the per-subscription subtree is served on
  `/onvif/events_service/sub/` with the same SOAP auth posture, the
  service actions ride the shared path-insensitive handler. The `alarm`
  SSE event is now advertised with AI active instead of requiring
  GB28181 — the alarm bridge exists whenever AI runs.

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
