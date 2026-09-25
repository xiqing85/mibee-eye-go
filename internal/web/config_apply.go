// config_apply.go decides how a persisted camera-section change applies:
// in-place camera pipeline restart (SPEC §5 applied:"camera_restart") or
// full process restart. Eligibility is conservative — only changes that
// provably leave every downstream surface intact may avoid the process
// restart.
package web

import (
	"github.com/xiqing85/mibee-eye-go/internal/camera"
	"github.com/xiqing85/mibee-eye-go/internal/config"
)

// cameraRestartEligible reports whether a camera-section change from
// `old` to `new` can apply via an in-place camera restart:
//
//   - effective (rotated) resolution unchanged → SPS geometry unchanged,
//     so RTSP streams, GB28181 media sessions, ONVIF announcements and
//     MSE clients see the same stream parameters (rotation transitions
//     within {0,180} or within {90,270}, plus flips);
//   - mode / fps / bitrate / codec / capture dims unchanged — those
//     either reshape the SPS (fps VUI timing), reopen different device
//     paths, or change announcements, and keep the proven full-restart
//     path.
func cameraRestartEligible(oldCfg, newCfg config.CameraConfig) bool {
	if oldCfg.Mode != newCfg.Mode {
		return false
	}
	if oldCfg.Width != newCfg.Width || oldCfg.Height != newCfg.Height {
		return false
	}
	if oldCfg.FPS != newCfg.FPS || oldCfg.Bitrate != newCfg.Bitrate || oldCfg.Codec != newCfg.Codec {
		return false
	}
	ow, oh := camera.RotatedDims(oldCfg.Width, oldCfg.Height, oldCfg.Rotation)
	nw, nh := camera.RotatedDims(newCfg.Width, newCfg.Height, newCfg.Rotation)
	return ow == nw && oh == nh
}
