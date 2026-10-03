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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
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

	// requestObserver, when set, receives each ONVIF SOAP action's local
	// name (e.g. "GetDeviceInformation") after the request completes —
	// wired to the Prometheus collector (SPEC appendix A #38). Nil-safe.
	requestObserver func(action string)
}

// SetRequestObserver installs the per-request action hook.
func (s *Server) SetRequestObserver(f func(action string)) {
	s.requestObserver = f
}

// New builds the composed server. advertiseIP is the device's own IP used
// verbatim as the host of every advertised URL (XAddrs, capabilities,
// stream/snapshot URIs); params and snapshot back the Imaging service and
// the /snapshot endpoint respectively. sub, when non-nil, adds the
// low-resolution substream as the second media profile (token `sub`,
// GetStreamUri routes it to the RTSP /sub mount — SPEC appendix A #20).
func New(cfg *config.Config, advertiseIP string, params *camera.ParamManager, snapshot *onvif.SnapshotBuffer, sub *camera.SubstreamInfo, opts ...onvifserver.Option) (*Server, error) {
	s := &Server{
		cfg:         cfg,
		snapshot:    snapshot,
		advertiseIP: advertiseIP,
	}

	libOpts := append([]onvifserver.Option{
		onvifserver.WithDeviceInfoProvider(&deviceInfoProvider{cfg: cfg}),
		onvifserver.WithStreamURIProvider(newStreamProvider(cfg.RTSP.Port)),
		onvifserver.WithImagingProvider(&imagingProvider{pm: params}),
		// Hardware-honest DeviceIO backend: without this the library's
		// simulator fabricates relay_1/relay_2/di_1 behind GetRelayOutputs
		// and GetDigitalInputs.
		onvifserver.WithRelayController(emptyRelayController{}),
	}, opts...)

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
		// Minimal Media2 (tr2) face on /onvif/media2_service + GetServices
		// entry (Profile-T entry path).
		SupportMedia2: cfg.ONVIF.Media2Enabled,
		// Alarm I/O family on the device service endpoint + GetServices
		// entry; the honest empty sets come from the injected relay
		// controller below (no relay/DI hardware on this device).
		SupportDeviceIO: cfg.ONVIF.DeviceIOEnabled,
		Profiles:        profilesFromConfig(cfg, sub),
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
	}, libOpts...)
	if err != nil {
		return nil, fmt.Errorf("onvif server: %w", err)
	}
	s.libServer = libServer

	s.soap = s.newSOAPHandler()

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
		sub := s.newSOAPHandler()
		sub.RegisterContextHandler("PullMessages", s.libServer.HandlePullMessages)
		sub.RegisterContextHandler("Renew", s.libServer.HandleRenew)
		sub.RegisterContextHandler("Unsubscribe", s.libServer.HandleUnsubscribe)
		mux.Handle("/onvif/events_service/sub/", sub)
	}
	if cfg.ONVIF.Media2Enabled {
		// Media2 rides its own path-bound handler: GetProfiles and
		// GetStreamUri repeat the Media1 action names, and the shared
		// handler dispatches by action name only — the subtree keeps
		// both faces answerable at their advertised endpoints (the
		// GetServices ver20/media XAddr is exactly this mount).
		media2 := s.newSOAPHandler()
		media2.RegisterContextHandler("GetProfiles", s.libServer.HandleMedia2GetProfiles)
		media2.RegisterContextHandler("GetStreamUri", s.libServer.HandleMedia2GetStreamUri)
		media2.RegisterContextHandler("SetSynchronizationPoint", s.libServer.HandleMedia2SetSynchronizationPoint)
		media2.RegisterContextHandler("GetVideoEncoderConfigurations", s.libServer.HandleMedia2GetVideoEncoderConfigurations)
		media2.RegisterContextHandler("GetServiceCapabilities", s.libServer.HandleMedia2GetServiceCapabilities)
		mux.Handle("/onvif/media2_service", media2)
	}
	mux.Handle("/", probeSniffer{soap: s.soap, probe: s.responder})

	s.mux = mux

	return s, nil
}

// newSOAPHandler builds a SOAP handler with the service credentials and
// response encoding settings — the shared all-action handler, the events
// subscription subtree, and the Media2 subtree all share this posture.
func (s *Server) newSOAPHandler() *onvifsoap.Handler {
	return onvifsoap.NewHandlerWithOptions(onvifsoap.HandlerOptions{
		Username:         s.cfg.ONVIF.Username,
		Password:         s.cfg.ONVIF.Password,
		Auth:             onvifsoap.DefaultAuthPolicy(),
		ExplicitPrefixes: true,
	})
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
	s.soap.RegisterContextHandler("GetVideoEncoderConfigurations", s.libServer.HandleGetVideoEncoderConfigurations)
	s.soap.RegisterContextHandler("GetVideoEncoderConfigurationOptions", s.libServer.HandleGetVideoEncoderConfigurationOptions)
	// ver10 sync point — the tr2 handler lives on the media2 subtree;
	// both fire the keyframe hook passed via New's opts.
	s.soap.RegisterContextHandler("SetSynchronizationPoint", s.libServer.HandleSetSynchronizationPoint)
	if s.snapshot.Enabled() {
		s.soap.RegisterContextHandler("GetSnapshotUri", s.libServer.HandleGetSnapshotUri)
	}

	// Media OSD configuration loop + audio configuration family. The OSD
	// store starts empty (no OSD engine on this device — entries exist
	// only after a client creates them, nothing is fabricated) and the
	// audio enumerations are empty because the hardware has no audio.
	s.soap.RegisterContextHandler("GetOSDs", s.libServer.HandleGetOSDs)
	s.soap.RegisterContextHandler("GetOSD", s.libServer.HandleGetOSD)
	s.soap.RegisterContextHandler("CreateOSD", s.libServer.HandleCreateOSD)
	s.soap.RegisterContextHandler("SetOSD", s.libServer.HandleSetOSD)
	s.soap.RegisterContextHandler("DeleteOSD", s.libServer.HandleDeleteOSD)
	s.soap.RegisterContextHandler("GetAudioSources", s.libServer.HandleGetAudioSources)
	s.soap.RegisterContextHandler("GetAudioSourceConfigurations", s.libServer.HandleGetAudioSourceConfigurations)
	s.soap.RegisterContextHandler("GetAudioEncoderConfigurations", s.libServer.HandleGetAudioEncoderConfigurations)
	s.soap.RegisterContextHandler("GetAudioOutputs", s.libServer.HandleGetAudioOutputs)
	s.soap.RegisterContextHandler("GetAudioDecoderConfigurations", s.libServer.HandleGetAudioDecoderConfigurations)

	// DeviceIO alarm I/O family (device service actions). The injected
	// emptyRelayController keeps every answer an honest empty set.
	if s.cfg.ONVIF.DeviceIOEnabled {
		s.soap.RegisterContextHandler("GetRelayOutputs", s.libServer.HandleGetRelayOutputs)
		s.soap.RegisterContextHandler("SetRelayOutputState", s.libServer.HandleSetRelayOutputState)
		s.soap.RegisterContextHandler("GetDigitalInputs", s.libServer.HandleGetDigitalInputs)
		s.soap.RegisterContextHandler("GetDeviceIOServiceCapabilities", s.libServer.HandleGetDeviceIOServiceCapabilities)
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

// observeRequests wraps the mux with the action counter: the SOAP
// envelope body is tee'd into a capped buffer while the handler reads,
// then the first Body child's local name is reported (best effort —
// non-SOAP probes like WS-Discovery count as "probe").
func (s *Server) observeRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.requestObserver == nil {
			next.ServeHTTP(w, r)
			return
		}
		var peek bytes.Buffer
		if r.Body != nil {
			r.Body = struct {
				io.Reader
				io.Closer
			}{
				Reader: io.TeeReader(io.LimitReader(r.Body, peekLimit), &peek),
				Closer: r.Body,
			}
		}
		next.ServeHTTP(w, r)
		s.requestObserver(soapActionFromEnvelope(peek.Bytes()))
	})
}

// peekLimit caps the envelope prefix kept for action extraction.
const peekLimit = 16 << 10

// soapActionFromEnvelope extracts the SOAP Body child's local name
// ("GetDeviceInformation" etc.); empty when nothing parseable was read.
func soapActionFromEnvelope(buf []byte) string {
	body := bytes.Index(buf, []byte(":Body>"))
	if body < 0 {
		body = bytes.Index(buf, []byte("Body>"))
		if body < 0 {
			return "probe"
		}
	}
	rest := buf[body:]
	lt := bytes.IndexByte(rest, '<')
	if lt < 0 {
		return "unknown"
	}
	tag := rest[lt+1:]
	end := bytes.IndexAny(tag, " >/")
	if end < 0 {
		return "unknown"
	}
	name := string(tag[:end])
	if i := strings.LastIndexByte(name, ':'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// Start starts the ONVIF HTTP server and blocks until ctx is cancelled or
// the listener fails.
func (s *Server) Start(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", s.cfg.ONVIF.Port)
	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           s.observeRequests(s.mux),
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

// profilesFromConfig lists the primary profile first (the NVR
// auto-selects the first profile), then the substream profile when the
// pipeline actually started.
func profilesFromConfig(cfg *config.Config, sub *camera.SubstreamInfo) []onvifserver.ProfileConfig {
	profiles := []onvifserver.ProfileConfig{profileFromConfig(cfg)}
	if sub != nil {
		profiles = append(profiles, onvifserver.ProfileConfig{
			Token: "sub",
			Name:  "sub",
			VideoSource: onvifserver.VideoSourceConfig{
				Token: "videoSrc0",
				Name:  "videoSrc0",
				Resolution: onvifserver.Resolution{
					Width:  sub.Width,
					Height: sub.Height,
				},
				Framerate: sub.FPS,
				Bounds: onvifserver.Bounds{
					X:      0,
					Y:      0,
					Width:  sub.Width,
					Height: sub.Height,
				},
			},
			VideoEncoder: onvifserver.VideoEncoderConfig{
				Encoding: "H264",
				Resolution: onvifserver.Resolution{
					Width:  sub.Width,
					Height: sub.Height,
				},
				Quality:   80,
				Framerate: sub.FPS,
				Bitrate:   sub.Bitrate,
				GovLength: sub.FPS * 2,
			},
		})
	}
	return profiles
}

// profileFromConfig builds the single media profile advertised by
// GetProfiles from the camera configuration. The NVR auto-selects the
// first profile and expects an H264 VideoEncoderConfiguration.
func profileFromConfig(cfg *config.Config) onvifserver.ProfileConfig {
	// Post-rotation stream resolution (SPEC appendix A #19): Profile S
	// must match the actual (transformed) stream aspect.
	w, h := cfg.Camera.EffectiveDims()
	return onvifserver.ProfileConfig{
		Token: "main",
		Name:  "main",
		VideoSource: onvifserver.VideoSourceConfig{
			Token: "videoSrc0",
			Name:  "videoSrc0",
			Resolution: onvifserver.Resolution{
				Width:  w,
				Height: h,
			},
			Framerate: cfg.Camera.FPS,
			Bounds: onvifserver.Bounds{
				X:      0,
				Y:      0,
				Width:  w,
				Height: h,
			},
		},
		VideoEncoder: onvifserver.VideoEncoderConfig{
			Encoding: "H264",
			Resolution: onvifserver.Resolution{
				Width:  w,
				Height: h,
			},
			Quality:   80,
			Framerate: cfg.Camera.FPS,
			Bitrate:   cfg.Camera.Bitrate, // raw config value, historical shape
			GovLength: cfg.Camera.IDRPeriod,
		},
		Snapshot: onvifserver.SnapshotConfig{
			Enabled: true,
			Resolution: onvifserver.Resolution{
				Width:  w,
				Height: h,
			},
		},
	}
}
