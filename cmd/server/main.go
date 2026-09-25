package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	gbdev "github.com/mickeyzzc/gb28181-go/device"
	"github.com/xiqing85/mibee-eye-go/internal/ai"
	"github.com/xiqing85/mibee-eye-go/internal/camera"
	"github.com/xiqing85/mibee-eye-go/internal/config"
	"github.com/xiqing85/mibee-eye-go/internal/gb35114auth"
	"github.com/xiqing85/mibee-eye-go/internal/gbalarm"
	"github.com/xiqing85/mibee-eye-go/internal/gbdate"
	"github.com/xiqing85/mibee-eye-go/internal/gbposition"
	"github.com/xiqing85/mibee-eye-go/internal/h264"
	"github.com/xiqing85/mibee-eye-go/internal/hls"
	"github.com/xiqing85/mibee-eye-go/internal/metrics"
	"github.com/xiqing85/mibee-eye-go/internal/netutil"
	"github.com/xiqing85/mibee-eye-go/internal/onvif"
	onvifgo "github.com/xiqing85/mibee-eye-go/internal/onvif/onvifgo"
	"github.com/xiqing85/mibee-eye-go/internal/recording"
	"github.com/xiqing85/mibee-eye-go/internal/rtmp"
	"github.com/xiqing85/mibee-eye-go/internal/rtsp"
	"github.com/xiqing85/mibee-eye-go/internal/web"
)

// ---------------------------------------------------------------------------
// GB28181 device-library adapters
// ---------------------------------------------------------------------------

// auHubFrameSource adapts the host h264.AUHub to the gb28181-go device
// package's FrameSource seam. Each subscription bridges the host channel,
// converting access units; cancelling the subscription context tears the
// bridge down with it.
type auHubFrameSource struct{ hub *h264.AUHub }

func (a auHubFrameSource) Subscribe(ctx context.Context) *gbdev.FrameSubscription {
	sub := a.hub.Subscribe(ctx)
	out := make(chan gbdev.AccessUnit, cap(sub.Channel))
	go func() {
		defer close(out)
		for au := range sub.Channel {
			nalus := make([]gbdev.NALU, len(au.NALUs))
			for i, n := range au.NALUs {
				nalus[i] = gbdev.NALU{Type: n.Type, Data: n.Data, IsIDR: n.IsIDR, IsSPS: n.IsSPS, IsPPS: n.IsPPS}
			}
			select {
			case out <- gbdev.AccessUnit{NALUs: nalus, Timestamp: au.Timestamp, KeyFrame: au.KeyFrame}:
			default: // slow consumer — drop, mirroring hub semantics
			}
		}
	}()
	return &gbdev.FrameSubscription{ID: sub.ID, Channel: out}
}

func (a auHubFrameSource) Unsubscribe(id string) { a.hub.Unsubscribe(id) }

// recordingIndexAdapter adapts the host recording index to the device
// package's PlaybackIndex seam (Lookup with SegmentMeta conversion + Root).
type recordingIndexAdapter struct {
	idx  *recording.Index
	root string
}

func (r recordingIndexAdapter) Lookup(startMs, endMs int64) []gbdev.SegmentMeta {
	segs := r.idx.Lookup(startMs, endMs)
	out := make([]gbdev.SegmentMeta, len(segs))
	for i, s := range segs {
		out[i] = gbdev.SegmentMeta{File: s.File, StartMS: s.StartMS, EndMS: s.EndMS, Size: s.Size, Frames: s.Frames, Keyframes: s.Keyframes}
	}
	return out
}

func (r recordingIndexAdapter) Root() string { return r.root }

// toDeviceConfig converts the host config structs (YAML-facing) to the
// device package's Config/DeviceInfo (field-identical).
func toDeviceConfig(c config.GB28181Config) gbdev.Config {
	return gbdev.Config{
		Enabled:               c.Enabled,
		PlatformSIPAddress:    c.PlatformSIPAddress,
		PlatformSIPPort:       c.PlatformSIPPort,
		DeviceID:              c.DeviceID,
		ChannelID:             c.ChannelID,
		SIPDomain:             c.SIPDomain,
		Password:              c.Password,
		LocalSIPPort:          c.LocalSIPPort,
		RegisterIntervalSecs:  c.RegisterIntervalSecs,
		HeartbeatIntervalSecs: c.HeartbeatIntervalSecs,
		HeartbeatTimeoutCount: c.HeartbeatTimeoutCount,
		Transport:             c.Transport,
	}
}

func toDeviceInfo(d config.DeviceConfig) gbdev.DeviceInfo {
	return gbdev.DeviceInfo{
		Name:         d.Name,
		Manufacturer: d.Manufacturer,
		Model:        d.Model,
		Firmware:     d.Firmware,
		HardwareID:   d.HardwareID,
		SerialNumber: d.SerialNumber,
	}
}

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func initLogging(level string) {
	var slevel slog.Level
	switch strings.ToLower(level) {
	case "debug":
		slevel = slog.LevelDebug
	case "warn":
		slevel = slog.LevelWarn
	case "error":
		slevel = slog.LevelError
	default:
		slevel = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slevel})
	slog.SetDefault(slog.New(web.NewSlogTeeHandler(handler, mainObserve)))
	// Also capture any stdlib `log` writers (SPEC §3.2 log ring).
	mainObserve.AttachLogRing()
}

// mainObserve is the process-wide observability state, created before
// logging starts so /api/logs captures everything (SPEC §3.2).
var mainObserve = web.NewObserve()

// configAdapter wraps config.Config to implement config.ConfigProvider.
type configAdapter struct {
	cfg      *config.Config
	deviceIP string
}

func (a *configAdapter) ONVIFUsername() string { return a.cfg.ONVIF.Username }
func (a *configAdapter) ONVIFPassword() string { return a.cfg.ONVIF.Password }
func (a *configAdapter) ONVIFPort() int        { return a.cfg.ONVIF.Port }
func (a *configAdapter) RTSPPort() int         { return a.cfg.RTSP.Port }
func (a *configAdapter) DeviceIP() string      { return a.deviceIP }
func (a *configAdapter) CameraDevice() string  { return a.cfg.Camera.Device }
func (a *configAdapter) CameraCodec() string   { return a.cfg.Camera.Codec }

// buildCameraFromConfig assembles the capture backend for cfg: camera
// params (config → defaults), CameraInfo, and the mode switch. Extracted
// so the in-place camera restart (SPEC §5 applied:"camera_restart") can
// rebuild the pipeline from freshly loaded config with identical logic.
func buildCameraFromConfig(cfg *config.Config) (camera.Camera, string) {
	cameraParams := camera.DefaultParams()
	cameraParams.Width = uint32(cfg.Camera.Width)
	cameraParams.Height = uint32(cfg.Camera.Height)
	cameraParams.FPS = float32(cfg.Camera.FPS)
	cameraParams.Bitrate = uint32(cfg.Camera.Bitrate)
	cameraParams.Brightness = float32(cfg.Camera.Brightness)
	cameraParams.Contrast = float32(cfg.Camera.Contrast)
	cameraParams.Saturation = float32(cfg.Camera.Saturation)
	cameraParams.Sharpness = float32(cfg.Camera.Sharpness)
	cameraParams.IDRPeriod = uint32(cfg.Camera.IDRPeriod)
	cameraParams.Codec = "hardwareH264"
	// Device-level flips from config: baked into the encoded stream by the
	// capture backend (libcamera transform), so every consumer (RTSP, ONVIF,
	// GB28181, recordings, /snapshot) sees them, persistently.
	cameraParams.HFlip = cfg.Camera.HFlip
	cameraParams.VFlip = cfg.Camera.VFlip
	cameraInfo := camera.CameraInfo{
		Name:         cfg.Device.Name,
		Manufacturer: cfg.Device.Manufacturer,
		Model:        cfg.Device.Model,
		SerialNumber: cfg.Device.SerialNumber,
		Width:        uint32(cfg.Camera.Width),
		Height:       uint32(cfg.Camera.Height),
		FPS:          float32(cfg.Camera.FPS),
		Codec:        "H264",
	}

	externalRTSPURL := ""
	var cam camera.Camera
	switch cfg.Camera.Mode {
	case "rtsp":
		externalRTSPURL = cfg.Camera.RTSPURL
		if externalRTSPURL == "" {
			externalRTSPURL = fmt.Sprintf("rtsp://127.0.0.1:%d/stream", cfg.RTSP.Port)
		}
		slog.Info("camera: using external RTSP source", "url", externalRTSPURL)
		cam = camera.NewRTSPSource(externalRTSPURL, cameraParams, cameraInfo)
	case "rpicamvid":
		// Uses the rpicam-vid binary from camera.vid_bin (PATH by default).
		// The configured bin_path stays pointed at mtxrpicam for fallback.
		slog.Info("camera: using rpicam-vid subprocess", "bin", cfg.Camera.VidBin)
		cam = camera.NewRPiCamVidCamera(
			camera.WithVidBinPath(cfg.Camera.VidBin),
			camera.WithVidParams(cameraParams),
			camera.WithVidInfo(cameraInfo),
			camera.WithVidFrameBufferSize(cfg.Camera.FrameBufferSize),
			// Rotation (SPEC appendix A #19): 0/180 bake via libcamera
			// flips; 90/270 switch the subprocess to raw YUV420 and
			// transpose in-process (M2M hardware encode, ffmpeg fallback).
			camera.WithVidRotation(cfg.Camera.Rotation),
			camera.WithVidEncoderDevice(cfg.Camera.EncoderDevice),
			camera.WithVidFFmpegBin(cfg.Camera.FFmpegBin),
		)
	case "v4l2":
		// Generic V4L2 backend (any board): pure-Go MMAP capture from
		// /dev/videoN; M2M hardware encode when encoder_device probes
		// capable, otherwise a resident ffmpeg subprocess.
		slog.Info("camera: using generic v4l2 backend", "device", cfg.Camera.Device,
			"encoder_device", cfg.Camera.EncoderDevice)
		cam = camera.NewV4L2Source(
			camera.WithV4L2Device(cfg.Camera.Device),
			camera.WithV4L2EncoderDevice(cfg.Camera.EncoderDevice),
			camera.WithV4L2FFmpegBin(cfg.Camera.FFmpegBin),
			camera.WithV4L2Params(cameraParams),
			camera.WithV4L2Info(cameraInfo),
			// Go-side pixel transpose before encoding (SPEC appendix A #19).
			camera.WithV4L2Rotation(cfg.Camera.Rotation),
		)
	default:
		cam = camera.NewRPiCamera(
			camera.WithBinPath(cfg.Camera.BinPath),
			camera.WithParams(cameraParams),
			camera.WithInfo(cameraInfo),
			camera.WithFrameBufferSize(cfg.Camera.FrameBufferSize),
		)
	}
	return cam, externalRTSPURL
}

func (a *configAdapter) CameraBitrate() int  { return a.cfg.Camera.Bitrate }
func (a *configAdapter) CameraWidth() int    { return a.cfg.Camera.Width }
func (a *configAdapter) CameraHeight() int   { return a.cfg.Camera.Height }
func (a *configAdapter) CameraFPS() int      { return a.cfg.Camera.FPS }
func (a *configAdapter) CameraHFlip() bool   { return a.cfg.Camera.HFlip }
func (a *configAdapter) CameraVFlip() bool   { return a.cfg.Camera.VFlip }
func (a *configAdapter) CameraRotation() int { return a.cfg.Camera.Rotation }
func (a *configAdapter) CameraEffectiveWidth() int {
	w, _ := a.cfg.Camera.EffectiveDims()
	return w
}
func (a *configAdapter) CameraEffectiveHeight() int {
	_, h := a.cfg.Camera.EffectiveDims()
	return h
}
func (a *configAdapter) DeviceName() string         { return a.cfg.Device.Name }
func (a *configAdapter) DeviceManufacturer() string { return a.cfg.Device.Manufacturer }
func (a *configAdapter) DeviceModel() string        { return a.cfg.Device.Model }
func (a *configAdapter) DeviceFirmware() string     { return a.cfg.Device.Firmware }
func (a *configAdapter) DeviceHardwareID() string   { return a.cfg.Device.HardwareID }
func (a *configAdapter) DeviceSerialNumber() string { return a.cfg.Device.SerialNumber }
func (a *configAdapter) LoggingLevel() string       { return a.cfg.Logging.Level }
func (a *configAdapter) SnapshotEnabled() bool      { return a.cfg.Snapshot.Enabled }
func (a *configAdapter) SnapshotQuality() int       { return a.cfg.Snapshot.Quality }

func main() {
	configPath := flag.String("config", "configs/config.yaml", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Fprintf(os.Stderr, "mibee-eye version %s\n", version)
		os.Exit(0)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	initLogging(cfg.Logging.Level)

	if cfg.ONVIF.Password == "" {
		slog.Error("ONVIF password must not be empty. Set onvif.password in config or MIBEE_EYE_ONVIF_PASSWORD env var")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	localIP := netutil.DetectLocalIP()
	slog.Info("MiBee Eye starting", "version", version, "fallback_ip", localIP)
	adapter := &configAdapter{cfg: cfg, deviceIP: localIP}

	// --- Step 1: Camera ---
	built, externalRTSPURL := buildCameraFromConfig(cfg)

	// In-place camera restarts (SPEC §5 applied:"camera_restart"): the
	// wrapper exposes a stable Frames() channel, so geometry-preserving
	// config changes swap the source without recycling the process —
	// GB28181 registration, ONVIF and web sessions all survive.
	cameraFactory := func(ctx context.Context) (camera.Camera, error) {
		fresh, err := config.Load(*configPath)
		if err != nil {
			return nil, err
		}
		c, _ := buildCameraFromConfig(fresh)
		return c, nil
	}
	restr := camera.NewRestartable(ctx, built, cameraFactory)
	var cam camera.Camera = restr

	if err := cam.Start(ctx); err != nil {
		slog.Error("camera start", "error", err)
		os.Exit(1)
	}

	metricsCollector := metrics.NewCollector()
	// --- Step 2: H264 Parser + AUHub ---
	parser := h264.NewParser()
	auHub := h264.NewAUHubWithSize(cfg.RTSP.SubscriberBufferSize)
	auHub.StartDropLogger(ctx)

	// SnapshotBuffer for /snapshot endpoint
	snapshotBuffer := onvif.NewSnapshotBuffer(true, cfg.Camera.StillBin, cfg.Camera.FFmpegBin)
	// Tier-1 rpicam-still snapshots apply the same transform the stream
	// bakes in (#9/#19) so the JPEG orientation always matches.
	snapshotBuffer.SetTransform(cfg.Camera.Rotation, cfg.Camera.HFlip, cfg.Camera.VFlip)
	go snapshotBuffer.SubscribeToHub(ctx, auHub)

	go func() {
		// Cache the latest SPS and PPS NALUs so we can inject them before
		// IDR frames that don't include them (mtxrpicam sends SPS/PPS only
		// on the first frame, not on subsequent IDR refreshes).
		var cachedSPS, cachedPPS []byte
		for frame := range cam.Frames() {
			metricsCollector.IncFramesCaptured()
			nalus := parser.Parse(frame.Data)
			if len(nalus) == 0 {
				continue
			}

			// Update SPS/PPS cache.
			hasSPS, hasPPS, hasIDR := false, false, false
			for _, n := range nalus {
				if n.IsSPS {
					cachedSPS = n.Data
					hasSPS = true
				}
				if n.IsPPS {
					cachedPPS = n.Data
					hasPPS = true
				}
				if n.IsIDR {
					hasIDR = true
				}
			}

			// Inject cached SPS+PPS before IDR if missing.
			if hasIDR && (!hasSPS || !hasPPS) && cachedSPS != nil && cachedPPS != nil {
				injected := make([]h264.NALU, 0, len(nalus)+2)
				if !hasSPS {
					injected = append(injected, h264.NALU{Type: 7, Data: cachedSPS, IsSPS: true})
				}
				if !hasPPS {
					injected = append(injected, h264.NALU{Type: 8, Data: cachedPPS, IsPPS: true})
				}
				nalus = append(injected, nalus...)
			}

			au := h264.AccessUnit{
				NALUs:     nalus,
				Timestamp: frame.Timestamp,
				KeyFrame:  hasIDR,
			}
			auHub.Write(au)
		}
	}()

	// --- Step 2.5: AI detection (SPEC v1 §4.6; fail-open) ---
	// Taps the AUHub (passive — never the capture/encode path): an ffmpeg
	// subprocess decodes keyframes, ONNX Runtime runs NanoDet inference.
	// NewService returns nil when disabled or unavailable.
	ai.InitRegistry("/var/lib/mibee-eye/models")
	// Post-rotation stream resolution (SPEC appendix A #19) — the AI
	// detection bbox space uses the effective dims.
	effW, effH := cfg.Camera.EffectiveDims()
	aiService := ai.NewService(ai.Options{
		Enabled:             cfg.AI.Enabled,
		Model:               cfg.AI.Model,
		ModelPath:           cfg.AI.ModelPath,
		OnnxLibPath:         cfg.AI.OnnxLibPath,
		ConfidenceThreshold: cfg.AI.ConfidenceThreshold,
		IntervalMs:          cfg.AI.IntervalMs,
		DecoderBin:          cfg.AI.DecoderBin,
		VideoW:              uint32(effW),
		VideoH:              uint32(effH),
	}, auHub, ai.NewDetector)
	// AI → alarm fan-out bridge (GB NOTIFY §9.5 + SPEC §6 SSE alarm +
	// ONVIF MotionAlarm): exists whenever AI runs; the GB sender is
	// attached once the GB server exists (Step 6.5) and the ONVIF sink
	// once the ONVIF server exists (Step 5). Each channel no-ops until
	// its consumer is up/subscribed.
	var alarmBridge *gbalarm.Bridge
	if aiService != nil {
		alarmBridge = gbalarm.New(cfg.GB28181.AlarmNotifyEnabled,
			time.Duration(cfg.GB28181.AlarmCooldownSecs)*time.Second)
		events := aiService.Events()
		go func() {
			for ev := range events {
				alarmBridge.OnDetections(time.Now().UnixMilli(), len(ev.Detections))
			}
		}()
	}
	if aiService != nil {
		aiService.Start(ctx)
	}

	// --- Step 3: RTSP Server (skipped when consuming external RTSP) ---
	var rtspServer *rtsp.Server
	if externalRTSPURL != "" {
	} else {
		rtspSub := auHub.Subscribe(ctx)
		rtspServer = rtsp.New(rtsp.Config{
			Port:           cfg.RTSP.Port,
			Username:       cfg.RTSP.Username,
			Password:       cfg.RTSP.Password,
			Address:        localIP,
			WriteQueueSize: cfg.RTSP.WriteQueueSize,
			EnableUDP:      cfg.RTSP.EnableUDP,
			UDPRTPPort:     cfg.RTSP.UDPRTPPort,
			UDPRTCPPort:    cfg.RTSP.UDPRTCPPort,
		})
		rtspServer.SetFrameSource(rtspSub.Channel)

		if err := rtspServer.Start(ctx); err != nil {
			slog.Error("rtsp server start", "error", err)
			os.Exit(1)
		}
	}

	// --- Step 4: ParamManager ---
	paramManager := camera.NewParamManager(cam)

	// --- Step 5: ONVIF Server (onvif-go/v2 transport) ---
	// Advertises the device's own IP (localIP) in every URL: the NVR consumes
	// XAddrs and stream URIs verbatim as this camera's endpoint.
	onvifServer, err := onvifgo.New(cfg, localIP, paramManager, snapshotBuffer)
	if err != nil {
		slog.Error("onvif server init", "error", err)
		os.Exit(1)
	}
	// ONVIF MotionAlarm rides the alarm bridge's accepted rising edges
	// (events disabled / no NVR subscription → no-ops inside).
	if alarmBridge != nil {
		alarmBridge.SetOnvifSink(func(_ int64, targets int) {
			onvifServer.PublishMotionAlarm(targets)
		})
	}

	// In-place camera restart hook for the web layer (SPEC §5
	// applied:"camera_restart"): swap the capture source and refresh the
	// snapshot still-transform to the freshly persisted rotation/flips.
	restartCamera := func() error {
		if err := restr.Restart(); err != nil {
			return err
		}
		if fresh, err := config.Load(*configPath); err == nil {
			snapshotBuffer.SetTransform(fresh.Camera.Rotation, fresh.Camera.HFlip, fresh.Camera.VFlip)
		}
		return nil
	}

	var webServer *web.Server
	// --- Step 5.5: Web UI Server ---
	if cfg.Web.Enabled {
		webServer = web.New(web.Config{
			Port:              cfg.Web.Port,
			RestartCamera:     restartCamera,
			Username:          cfg.Web.Username,
			Password:          cfg.Web.Password,
			ConfigPath:        *configPath,
			OnvifConfig:       adapter,
			GB28181Config:     &cfg.GB28181,
			Params:            paramManager,
			AUHub:             auHub,
			AI:                aiService,
			ReadHeaderTimeout: cfg.Web.ReadHeaderTimeout,
			ReadTimeout:       cfg.Web.ReadTimeout,
			WriteTimeout:      cfg.Web.WriteTimeout,
			IdleTimeout:       cfg.Web.IdleTimeout,
			Snapshot:          snapshotBuffer,
			Metrics:           metricsCollector,
		})
		// SPEC v1 §6 `alarm` SSE: the GB28181 alarm bridge's accepted
		// rising edges also reach the web event hub (web disabled → no
		// listener installed, the NOTIFY path is unaffected).
		if alarmBridge != nil {
			hub := webServer
			alarmBridge.SetEventSink(func(nowMs int64, targets int) {
				hub.BroadcastEvent("alarm", map[string]any{
					"camera_id": "0",
					"active":    true,
					"source":    "ai",
					"targets":   targets,
					"timestamp": nowMs,
				})
			})
		}
		// Observability (SPEC §3.2): resource snapshot sampler + log ring.
		go webServer.Observe().RunSampler(ctx, 2*time.Second)
		go func() {
			if err := webServer.Start(ctx); err != nil {
				slog.Warn("web server exited", "error", err)
			}
		}()
	}

	// --- Step 5.75: HLS Server ---
	var hlsServer *hls.Server
	if cfg.HLS.Enabled {
		hlsServer = hls.New(hls.Config{
			Hub:             auHub,
			SegmentDuration: cfg.HLS.SegmentDuration,
			FPS:             cfg.Camera.FPS,
		})
		go hlsServer.Start(ctx)

		// Register HLS HTTP routes on the web server's mux if available,
		// otherwise start a standalone HTTP server for HLS.
		if webServer != nil {
			// HLS routes share the web server's port
			hlsServer.RegisterHTTP(webServer.Mux())
		} else {
			// Start a minimal HLS HTTP server on a separate port (8089)
			hlsMux := http.NewServeMux()
			hlsServer.RegisterHTTP(hlsMux)
			hlsSrv := &http.Server{
				Addr:              ":8089",
				Handler:           hlsMux,
				ReadHeaderTimeout: 5 * time.Second,
				ReadTimeout:       10 * time.Second,
				WriteTimeout:      0, // disable for streaming
				IdleTimeout:       30 * time.Second,
			}
			go func() {
				slog.Info("hls: HTTP server starting", "port", 8089)
				if err := hlsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					slog.Warn("hls: HTTP server exited", "error", err)
				}
			}()
			defer hlsSrv.Close()
		}
		slog.Info("hls: enabled", "segment_duration", cfg.HLS.SegmentDuration, "fps", cfg.Camera.FPS)
	} else {
		slog.Info("hls: disabled")
	}

	// --- Step 5.8: RTMP Push ---
	var rtmpPush *rtmp.Push
	if cfg.RTMP.Enabled {
		rtmpPush = rtmp.New(rtmp.Config{
			URL:        cfg.RTMP.URL,
			Hub:        auHub,
			MaxRetries: cfg.RTMP.MaxRetries,
		})
		if err := rtmpPush.Start(ctx); err != nil {
			slog.Error("rtmp: push start failed", "error", err)
			os.Exit(1)
		}
		slog.Info("rtmp: push enabled", "url", cfg.RTMP.URL)
	} else {
		slog.Info("rtmp: push disabled")
	}

	// --- Step 6: WS-Discovery (onvif-go/v2 responder: UDP multicast + HTTP probe) ---
	if err := onvifServer.StartDiscovery(ctx); err != nil {
		slog.Warn("warning: failed to start WS-Discovery", "error", err)
	}

	// Start ONVIF HTTP server in goroutine
	go func() {
		if err := onvifServer.Start(ctx); err != nil {
			slog.Warn("onvif server exited", "error", err)
		}
	}()

	// --- Step 6.4: Local recording ---
	var recWriter *recording.Writer
	if cfg.Recording.Enabled {
		recWriter = recording.NewWriter(auHub, cfg.Recording)
		gbdev.RecordActive = recWriter.Active
		go func() {
			if err := recWriter.Run(ctx); err != nil {
				slog.Error("recording: writer exited", "error", err)
			}
		}()
		slog.Info("recording: enabled", "path", cfg.Recording.StoragePath, "segment_secs", cfg.Recording.SegmentSecs, "retention_days", cfg.Recording.RetentionDays, "max_storage_mb", cfg.Recording.MaxStorageMB)
		ret := recording.NewRetention(cfg.Recording, recWriter.Index(), cfg.Recording.StoragePath)
		go ret.Run(ctx, 10*time.Minute)
	} else {
		slog.Info("recording: disabled")
	}

	// --- Step 6.5: GB/T 28181 device ---
	var gbServer *gbdev.Server
	if cfg.GB28181.Enabled {
		// gb28181-go v0.3.0 defaults the SIP User-Agent to a neutral value;
		// stamp this product's identity explicitly before New (concurrent
		// mutation afterwards would race the message builders).
		gbdev.UserAgent = fmt.Sprintf("mibee-eye-raspi-go/%s", version)
		devCfg := toDeviceConfig(cfg.GB28181)
		// GB 35114 A-level: replaces Digest auth when configured (and the
		// binary carries -tags gb35114). Errors are fatal — running with a
		// half-configured security identity would silently downgrade.
		authenticator, err := gb35114auth.Build(cfg.GB28181.GB35114, cfg.GB28181.DeviceID)
		if err != nil {
			log.Fatalf("gb28181: %v", err)
		}
		devCfg.RegisterAuthenticator = authenticator
		gbServer = gbdev.New(devCfg, toDeviceInfo(cfg.Device), auHubFrameSource{hub: auHub})
		// Wire the recording index for RecordInfo queries (nil when recording disabled).
		if recWriter != nil {
			gbServer.SetRecordingIndex(recordingIndexAdapter{idx: recWriter.Index(), root: cfg.Recording.StoragePath})
		}
		// Execute platform snapshot commands (GB/T 28181-2022 A.2.1.24):
		// capture via the /snapshot tiers, POST each JPEG to the
		// command's UploadURL; the library reports completion.
		gbServer.SetSnapshotExecutor(&onvif.SnapshotUploader{SB: snapshotBuffer})
		// AI alarms + the DeviceConfig(AlarmReport) runtime gate.
		if alarmBridge != nil {
			alarmBridge.SetSender(gbServer.Notifier())
			gbServer.SetConfigHandlers(gbdev.ConfigCallbacks{
				OnAlarmReport: func(motion, field uint32) {
					alarmBridge.SetMotionReporting(motion == 1)
				},
				OnFrameMirror: func(mode uint32) {
					h, v := gbalarm.MirrorModeToFlips(mode)
					if err := cam.SetParam("hFlip", h); err != nil {
						slog.Warn("gb28181: FrameMirror hFlip", "error", err)
					}
					if err := cam.SetParam("vFlip", v); err != nil {
						slog.Warn("gb28181: FrameMirror vFlip", "error", err)
					}
					slog.Info("gb28181: FrameMirror applied",
						"mode", mode, "hFlip", h, "vFlip", v)
				},
			})
		}
		// Static surveyed coordinates → MobilePosition NOTIFYs (§9.5.3)
		// while a platform subscribes; unset = no source.
		if cfg.GB28181.Longitude != "" && cfg.GB28181.Latitude != "" {
			gbServer.SetPositionSource(&gbposition.StaticPosition{
				Longitude: cfg.GB28181.Longitude,
				Latitude:  cfg.GB28181.Latitude,
			})
		}
		// DeviceControl(IFrameCmd Send): force an IDR on the next encoded
		// frame when the active source supports it (v4l2 mode's M2M
		// encoder does; subprocess sources like rpicamvid cannot be
		// signaled mid-stream and log once).
		var idrUnsupportedLogged bool
		gbServer.SetControlHandlers(gbdev.ControlCallbacks{
			OnForceIFrame: func() {
				forcer, ok := cam.(interface{ ForceIDR() error })
				if !ok {
					if !idrUnsupportedLogged {
						idrUnsupportedLogged = true
						slog.Warn("gb28181: IFrameCmd ignored — camera source does not support runtime IDR requests")
					}
					return
				}
				if err := forcer.ForceIDR(); err != nil {
					slog.Warn("gb28181: IFrameCmd force IDR failed", "error", err)
				}
			},
			// GB/T 28181 RecordCmd (§9.3.2): platform-requested manual
			// recording. StopRecord pauses the (config-enabled) recorder's
			// segment writing; Record resumes at a fresh segment boundary.
			// With recording disabled there is no writer to gate — logged.
			OnRecordCmd: func(start bool) {
				if recWriter == nil {
					slog.Warn("gb28181: RecordCmd ignored — recording disabled in config")
					return
				}
				recWriter.SetPaused(!start)
			},
		})
		go func() {
			if err := gbServer.Start(ctx); err != nil {
				slog.Error("gb28181 server", "error", err)
			}
		}()
		// SIP-Date drift observation (§9.10.2): poll the platform clock
		// from the REGISTER responses — observation only, the system
		// clock is never adjusted (disciplining stays a host/NTP call).
		go observePlatformDate(ctx, gbServer)
		slog.Info("gb28181: starting", "port", cfg.GB28181.LocalSIPPort)
	}

	// --- Step 7: Metrics ---
	if cfg.Metrics.Enabled {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", metricsCollector)
		metricsServer := &http.Server{
			Addr:              fmt.Sprintf(":%d", cfg.Metrics.Port),
			Handler:           metricsMux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
		}
		go func() {
			slog.Info("metrics: server starting", "port", cfg.Metrics.Port)
			if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Warn("metrics: server exited", "error", err)
			}
		}()
		defer metricsServer.Close()

		// Poll loop: snapshot camera drops, AUHub drops, RTSP clients, camera alive
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					metricsCollector.SetFramesDropped(auHub.DroppedAUs())
					if rtspServer != nil {
						metricsCollector.SetRTSPClients(rtspServer.ClientCount())
					}
					// Camera auto-restarts on failure, so it's effectively always alive
					// once Start() succeeds. Set to 1 unconditionally.
					metricsCollector.SetCameraAlive(true)
					if aiService != nil {
						metricsCollector.SetAIInferences(aiService.Inferences())
					}
				}
			}
		}()
		slog.Info("metrics: enabled", "port", cfg.Metrics.Port)
	} else {
		slog.Info("metrics: disabled")
	}

	shutdownStep("hls", 5*time.Second, func() error { return nil })
	<-ctx.Done()
	slog.Info("MiBee Eye shutting down", "version", version)
	shutdownStep("discovery", 5*time.Second, func() error { onvifServer.StopDiscovery(); return nil })
	shutdownStep("onvif", 5*time.Second, func() error { return onvifServer.Stop() })

	if gbServer != nil {
		// Graceful deregistration first (REGISTER Expires: 0): the
		// platform learns the departure now instead of via keepalive
		// timeout. A lost platform simply times the attempt out — log
		// and continue to the hard stop.
		deregCtx, deregCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := gbServer.Deregister(deregCtx); err != nil {
			slog.Warn("gb28181: deregistration not confirmed", "error", err)
		}
		deregCancel()
		gbServer.Stop()
		slog.Info("gb28181: stopped")
	}
	shutdownStep("web", 5*time.Second, func() error {
		if webServer != nil {
			return webServer.Stop()
		}
		return nil
	})
	shutdownStep("rtsp", 5*time.Second, func() error {
		if rtspServer != nil {
			return rtspServer.Stop()
		}
		return nil
	})
	shutdownStep("rtmp", 5*time.Second, func() error {
		if rtmpPush != nil {
			return rtmpPush.Stop()
		}
		return nil
	})
	shutdownStep("camera", 5*time.Second, func() error { return cam.Stop() })

	slog.Info("MiBee Eye stopped", "version", version)
}

// observePlatformDate polls the platform clock carried by the REGISTER
// response's SIP Date header (GB/T 28181-2022 §9.10.2) and warns on
// drift per the gbdate three-state latch. Pure observation — the clock
// is never adjusted here.
func observePlatformDate(ctx context.Context, gb *gbdev.Server) {
	ticker := time.NewTicker(gbdate.ObserveInterval)
	defer ticker.Stop()
	var last gbdate.LastWarned
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		platform := gb.PlatformDateUnix()
		if platform == 0 {
			continue // no REGISTER response seen yet
		}
		outcome, drift, newLast := gbdate.Evaluate(platform, time.Now().Unix(), last)
		switch outcome {
		case gbdate.Warn:
			slog.Warn("gb28181: platform clock drifts from local (SIP Date, §9.10.2); observation only — the system clock is not adjusted",
				"drift_secs", drift, "threshold_secs", gbdate.WarnDriftSecs)
		case gbdate.Recovered:
			slog.Info("gb28181: platform clock drift back within threshold")
		case gbdate.Stable:
		}
		last = newLast
	}
}

// shutdownStep runs a shutdown function with a timeout.
// If the function does not complete within the timeout, a warning is logged
// and execution continues to the next step.
func shutdownStep(name string, timeout time.Duration, fn func() error) {
	slog.Info("shutdown: stopping", "component", name)
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	done := make(chan error, 1)
	go func() {
		done <- fn()
	}()

	select {
	case err := <-done:
		if err != nil {
			slog.Warn("shutdown: stopped with error", "component", name, "error", err)
		} else {
			slog.Info("shutdown: stopped", "component", name)
		}
	case <-timer.C:
		slog.Warn("shutdown: stop timed out", "component", name, "timeout", timeout)
	}
}
