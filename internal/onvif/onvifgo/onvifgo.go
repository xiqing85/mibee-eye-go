// Package onvifgo composes the onvif-go/v2 server transport with MiBee
// Eye's real state sources (config, camera.ParamManager, SnapshotBuffer).
//
// The library's Server.Start() builds a fixed internal mux (one endpoint per
// service, no Probe interception, stdout logging), which does not fit this
// service. Instead this package composes the exported parts:
//
//   - one shared soap.Handler carries every action on every path, matching
//     the historical path-insensitive dispatch the MiBee NVR's raw SOAP
//     fallback depends on;
//   - WS-Discovery HTTP probes are routed to the discovery responder by a
//     pre-sniffing wrapper mounted in front of the SOAP handler;
//   - GET /snapshot is served by the dual-tier SnapshotBuffer;
//   - AdvertiseHost is pinned to the device's own IP: the NVR consumes
//     XAddrs and stream URIs verbatim as the camera's endpoint, so the
//     library's client-IP echo default must never be left enabled here.
package onvifgo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/xiqing85/mibee-eye-go/internal/camera"
	"github.com/xiqing85/mibee-eye-go/internal/config"
	"github.com/xiqing85/mibee-eye-go/internal/onvif"

	onvifserver "github.com/mickeyzzc/onvif-go/v2/server"
	onvifdiscovery "github.com/mickeyzzc/onvif-go/v2/server/discovery"
	onvifsoap "github.com/mickeyzzc/onvif-go/v2/server/soap"
)

// Server is the ONVIF SOAP + WS-Discovery server for MiBee Eye, backed by
// the onvif-go/v2 transport.
type Server struct {
	cfg         *config.Config
	libServer   *onvifserver.Server
	soap        *onvifsoap.Handler
	responder   *onvifdiscovery.Responder
	snapshot    *onvif.SnapshotBuffer
	advertiseIP string
	httpServer  *http.Server
	mux         *http.ServeMux
}

// New builds the composed server. advertiseIP is the device's own IP used
// verbatim as the host of every advertised URL (XAddrs, capabilities,
// stream/snapshot URIs); params and snapshot back the Imaging service and
// the /snapshot endpoint respectively.
func New(cfg *config.Config, advertiseIP string, params *camera.ParamManager, snapshot *onvif.SnapshotBuffer) (*Server, error) {
	s := &Server{
		cfg:         cfg,
		snapshot:    snapshot,
		advertiseIP: advertiseIP,
	}

	libServer, err := onvifserver.New(&onvifserver.Config{
		Host:     "0.0.0.0",
		Port:     cfg.ONVIF.Port,
		BasePath: "/onvif",
		// onvif-go rc6 fail-fasts on Timeout <= 0 (its Config.Validate,
		// issue #63) — keep the library default explicit.
		Timeout: 30 * time.Second,
		DeviceInfo: onvifserver.DeviceInfo{
			Manufacturer:    cfg.Device.Manufacturer,
			Model:           cfg.Device.Model,
			FirmwareVersion: cfg.Device.Firmware,
			SerialNumber:    cfg.Device.SerialNumber,
			HardwareID:      cfg.Device.HardwareID,
		},
		Username:         cfg.ONVIF.Username,
		Password:         cfg.ONVIF.Password,
		AdvertiseHost:    advertiseIP,
		ExplicitPrefixes: true,
		SupportPTZ:       false, // NVR expects PTZ: false
		SupportImaging:   true,
		// Pull-Point events service (AI MotionAlarm; key gates the
		// GetCapabilities XAddr advertisement too).
		SupportEvents: cfg.ONVIF.EventsEnabled,
		Profiles:      []onvifserver.ProfileConfig{profileFromConfig(cfg)},
		// GetScopes answers these (#37). Superset of the discovery scopes:
		// ProbeMatches carries only name+hardware (byte-stable for the NVR),
		// GetScopes additionally advertises the encoder type.
		Scopes: []string{
			"onvif://www.onvif.org/type/video_encoder",
			"onvif://www.onvif.org/name/" + deviceNameOrDefault(cfg),
			"onvif://www.onvif.org/hardware/" + hardwareIDOrDefault(cfg),
		},
		// Snapshot endpoint shape (#36): historical parameterless /snapshot.
		SnapshotPath:             "/snapshot",
		SnapshotURIParameterless: true,
	},
		onvifserver.WithDeviceInfoProvider(&deviceInfoProvider{cfg: cfg}),
		onvifserver.WithStreamURIProvider(newStreamProvider(cfg.RTSP.Port)),
		onvifserver.WithImagingProvider(&imagingProvider{pm: params}),
	)
	if err != nil {
		return nil, fmt.Errorf("onvif server: %w", err)
	}
	s.libServer = libServer

	s.soap = onvifsoap.NewHandlerWithOptions(onvifsoap.HandlerOptions{
		Username:         cfg.ONVIF.Username,
		Password:         cfg.ONVIF.Password,
		Auth:             onvifsoap.DefaultAuthPolicy(),
		ExplicitPrefixes: true,
	})

	s.registerActions()
	s.responder = s.newResponder()

	mux := http.NewServeMux()
	if snapshot.Enabled() {
		mux.Handle("/snapshot", snapshot)
	}
	if cfg.ONVIF.EventsEnabled {
		// Per-subscription subtree: the SubscriptionReference address
		// embeds the pull-point id in the path, so PullMessages /
		// Renew / Unsubscribe need their own (path-bound) handler.
		// Same auth posture as the shared SOAP handler.
		sub := onvifsoap.NewHandlerWithOptions(onvifsoap.HandlerOptions{
			Username:         cfg.ONVIF.Username,
			Password:         cfg.ONVIF.Password,
			Auth:             onvifsoap.DefaultAuthPolicy(),
			ExplicitPrefixes: true,
		})
		sub.RegisterContextHandler("PullMessages", s.libServer.HandlePullMessages)
		sub.RegisterContextHandler("Renew", s.libServer.HandleRenew)
		sub.RegisterContextHandler("Unsubscribe", s.libServer.HandleUnsubscribe)
		mux.Handle("/onvif/events_service/sub/", sub)
	}
	mux.Handle("/", probeSniffer{soap: s.soap, probe: s.responder})

	s.mux = mux

	return s, nil
}

// registerActions registers every supported action on the shared SOAP
// handler. Any action answers on any path (historical dispatch semantics).
// SystemReboot and Move are intentionally not registered: this device has
// no remote-reboot or focus hardware behind them.
func (s *Server) registerActions() {
	// Device service.
	s.soap.RegisterContextHandler("GetDeviceInformation", s.libServer.HandleGetDeviceInformation)
	s.soap.RegisterContextHandler("GetCapabilities", s.libServer.HandleGetCapabilities)
	s.soap.RegisterContextHandler("GetSystemDateAndTime", s.libServer.HandleGetSystemDateAndTime)
	s.soap.RegisterContextHandler("GetServices", s.libServer.HandleGetServices)
	s.soap.RegisterContextHandler("GetScopes", s.libServer.HandleGetScopes)

	// Media service.
	s.soap.RegisterContextHandler("GetProfiles", s.libServer.HandleGetProfiles)
	s.soap.RegisterContextHandler("GetStreamUri", s.libServer.HandleGetStreamUri)
	s.soap.RegisterContextHandler("GetVideoSources", s.libServer.HandleGetVideoSources)
	if s.snapshot.Enabled() {
		s.soap.RegisterContextHandler("GetSnapshotUri", s.libServer.HandleGetSnapshotUri)
	}

	// Imaging service.
	s.soap.RegisterContextHandler("GetImagingSettings", s.libServer.HandleGetImagingSettings)
	s.soap.RegisterContextHandler("SetImagingSettings", s.libServer.HandleSetImagingSettings)
	s.soap.RegisterContextHandler("GetOptions", s.libServer.HandleGetOptions)

	// Events service (path-insensitive like every other action); the
	// per-subscription subtree gets its own handler in New — see the
	// /onvif/events_service/sub/ mux entry.
	if s.cfg.ONVIF.EventsEnabled {
		s.soap.RegisterContextHandler("GetServiceCapabilities", s.libServer.HandleGetEventServiceCapabilities)
		s.soap.RegisterContextHandler("GetEventProperties", s.libServer.HandleGetEventProperties)
		s.soap.RegisterContextHandler("CreatePullPointSubscription", s.libServer.HandleCreatePullPointSubscription)
	}
}

// PublishMotionAlarm fans one accepted AI rising edge into the ONVIF
// events service (topic tns1:VideoSource/MotionAlarm). No subscriber —
// events disabled, or nobody pulled a SubscriptionReference yet — is a
// safe no-op. The shape mirrors the rs/notebook twins: Source is the
// SPEC single-camera id, State=true marks the rise, Targets carries
// the detection count.
func (s *Server) PublishMotionAlarm(targets int) {
	s.libServer.PublishEvent(onvifserver.Event{
		Topic: "tns1:VideoSource/MotionAlarm",
		Source: []onvifserver.SimpleItem{
			{Name: "Source", Value: "0"},
		},
		Data: []onvifserver.SimpleItem{
			{Name: "State", Value: "true"},
			{Name: "Targets", Value: strconv.Itoa(targets)},
		},
	})
}

// newResponder builds the WS-Discovery responder. XAddrs are pinned to the
// device's own address: the NVR uses ProbeMatches XAddrs verbatim as the
// camera endpoint, so the responder's per-requester derivation must stay
// off. The Types and Scopes strings match the historical ProbeMatches
// bytes (tdn: prefixes, name + hardware scopes).
func (s *Server) newResponder() *onvifdiscovery.Responder {
	name := s.cfg.Device.Name
	if name == "" {
		name = "Pi Camera V1"
	}
	hw := s.cfg.Device.HardwareID
	if hw == "" {
		hw = "OV5647"
	}

	return onvifdiscovery.NewResponder(onvifdiscovery.Config{
		// Historical identity format ("uuid:<uuid>", no urn: prefix) so the
		// NVR keeps treating reboots as the same device.
		EndpointRef: "uuid:" + uuid.New().String(),
		Types:       []string{"tdn:NetworkVideoTransmitter", "tdn:Device"},
		Scopes: []string{
			"onvif://www.onvif.org/name/" + name,
			"onvif://www.onvif.org/hardware/" + hw,
		},
		XAddrs: []string{
			fmt.Sprintf("http://%s:%d/onvif/device_service", s.advertiseIP, s.cfg.ONVIF.Port),
		},
		Port:       s.cfg.ONVIF.Port,
		DevicePath: "/onvif/device_service",
	})
}

// StartDiscovery starts the UDP multicast responder (which also announces
// Hello). Directed HTTP probes are answered through the main server's mux.
func (s *Server) StartDiscovery(ctx context.Context) error {
	if err := s.responder.Start(ctx); err != nil {
		return fmt.Errorf("onvif: discovery responder: %w", err)
	}
	slog.Info("onvif: discovery responder started")
	return nil
}

// StopDiscovery stops the UDP responder (sending Bye).
func (s *Server) StopDiscovery() {
	s.responder.Stop()
}

// Start starts the ONVIF HTTP server and blocks until ctx is cancelled or
// the listener fails.
func (s *Server) Start(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", s.cfg.ONVIF.Port)
	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("onvif: server starting", "addr", addr)
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		return s.Stop()
	case err := <-errCh:
		return err
	}
}

// Stop closes the ONVIF HTTP server.
func (s *Server) Stop() error {
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Close()
}

// profileFromConfig builds the single media profile advertised by
// GetProfiles from the camera configuration. The NVR auto-selects the
// first profile and expects an H264 VideoEncoderConfiguration.
func profileFromConfig(cfg *config.Config) onvifserver.ProfileConfig {
	return onvifserver.ProfileConfig{
		Token: "main",
		Name:  "main",
		VideoSource: onvifserver.VideoSourceConfig{
			Token: "videoSrc0",
			Name:  "videoSrc0",
			Resolution: onvifserver.Resolution{
				Width:  cfg.Camera.Width,
				Height: cfg.Camera.Height,
			},
			Framerate: cfg.Camera.FPS,
			Bounds: onvifserver.Bounds{
				X:      0,
				Y:      0,
				Width:  cfg.Camera.Width,
				Height: cfg.Camera.Height,
			},
		},
		VideoEncoder: onvifserver.VideoEncoderConfig{
			Encoding: "H264",
			Resolution: onvifserver.Resolution{
				Width:  cfg.Camera.Width,
				Height: cfg.Camera.Height,
			},
			Quality:   80,
			Framerate: cfg.Camera.FPS,
			Bitrate:   cfg.Camera.Bitrate, // raw config value, historical shape
			GovLength: cfg.Camera.IDRPeriod,
		},
		Snapshot: onvifserver.SnapshotConfig{
			Enabled: true,
			Resolution: onvifserver.Resolution{
				Width:  cfg.Camera.Width,
				Height: cfg.Camera.Height,
			},
		},
	}
}
