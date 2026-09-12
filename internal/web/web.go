package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/ai"
	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/camera"
	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/config"
	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/h264"

	"gopkg.in/yaml.v3"
)

// Config holds the web server configuration.
type Config struct {
	Port              int                   // listen port (default 8088)
	Username          string                // admin user (default = onvif user)
	Password          string                // admin pass (default = onvif pass)
	AllowedOrigins    []string              // CORS allowed origins (default ["*"])
	ConfigPath        string                // path to config.yaml (used by PUT /api/config)
	OnvifConfig       config.ConfigProvider // read-only onvif/rtsp config
	GB28181Config     *config.GB28181Config // GB28181 configuration
	Params            *camera.ParamManager  // imaging parameter manager
	AUHub             *h264.AUHub           // H.264 access-unit hub
	AI                *ai.Service           // AI detection service (nil = disabled/unavailable)
	Version           string                // build version from ldflags
	Logger            *log.Logger           // nil -> log.Default()
	ReadHeaderTimeout time.Duration         // http.Server.ReadHeaderTimeout (0 = default 5s)
	ReadTimeout       time.Duration         // http.Server.ReadTimeout (0 = default 10s)
	WriteTimeout      time.Duration         // http.Server.WriteTimeout (0 = streaming endpoints)
	IdleTimeout       time.Duration         // http.Server.IdleTimeout (0 = default 120s)
	CameraStatus      func() bool           // returns camera alive status (nil = unavailable)
	RTSPStatus        func() bool           // returns RTSP server status (nil = unavailable)
	FrameRate         func() float64        // returns current fps (nil = unavailable)
	Snapshot          http.Handler          // GET /snapshot handler (nil = disabled)
	Metrics           http.Handler          // GET /metrics handler (nil = disabled; SPEC §3.2)
}

// Server is the web UI HTTP server.
type Server struct {
	cfg          Config
	logger       *log.Logger
	mux          *http.ServeMux
	hub          *sseHub
	loginLimiter *loginRateLimiter
	server       *http.Server

	mu             sync.RWMutex // guards username/password
	username       string
	password       string
	sessions       *SessionStore
	allowedOrigins []string
	startTime      time.Time
	// Process restart for PUT /api/config and POST /api/system/restart
	// (SPEC §5.1). Injectable so tests can observe instead of dying.
	selfRestart func()
	// Observability state (SPEC §3.2).
	observe *Observe
}

// New creates a new web server. An empty password means first-boot state
// (POST /api/auth/setup configures the admin).
func New(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}

	username := cfg.Username
	password := cfg.Password
	if username == "" && cfg.OnvifConfig != nil {
		username = cfg.OnvifConfig.ONVIFUsername()
	}
	if password == "" && cfg.OnvifConfig != nil {
		password = cfg.OnvifConfig.ONVIFPassword()
	}

	origins := cfg.AllowedOrigins
	if len(origins) == 0 {
		origins = []string{"*"}
	}

	// Sessions persist next to the config file so the deliberate
	// self-restarts (config save, flips, /api/system/restart) keep
	// browsers signed in (SPEC 附录A #10). Best effort — a load failure
	// falls back to an in-memory store.
	sessions := NewSessionStore()
	if cfg.ConfigPath != "" {
		sessions = NewSessionStoreAt(filepath.Join(filepath.Dir(cfg.ConfigPath), "web-sessions.json"))
	}

	return &Server{
		cfg:            cfg,
		observe:        NewObserve(),
		hub:            newSSEHub(logger),
		logger:         logger,
		username:       username,
		password:       password,
		sessions:       sessions,
		allowedOrigins: origins,
		loginLimiter:   &loginRateLimiter{attempts: make(map[string]*rateLimitEntry)},
		selfRestart: func() {
			go func() {
				<-time.After(500 * time.Millisecond)
				_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
			}()
		},
	}
}

// Start starts the web UI HTTP server on the configured port.
func (s *Server) Start(ctx context.Context) error {
	port := s.cfg.Port
	if port == 0 {
		port = 8088
	}

	s.mux = http.NewServeMux()
	if s.hub == nil {
		// New() normally wires the hub; keep Start() self-sufficient too.
		s.hub = newSSEHub(s.logger)
	}

	// Imaging parameter changes → SSE param_changed events (SPEC §6).
	if s.cfg.Params != nil {
		s.cfg.Params.SetOnChange(func(name string, value interface{}) {
			s.hub.broadcast("param_changed", map[string]interface{}{
				"camera_id": "0",
				"name":      name,
				"value":     value,
			})
		})
	}

	// AI detections → SSE ai_detection events (SPEC §6).
	if s.cfg.AI != nil && s.cfg.AI.Active() {
		events := s.cfg.AI.Events()
		go func() {
			for evt := range events {
				s.hub.broadcast("ai_detection", evt)
			}
		}()
		s.logger.Printf("web: ai detection events enabled (model: %s)", s.cfg.AI.ModelName())
	}

	s.startTime = time.Now()
	s.registerRoutes()

	if !s.configured() {
		s.logger.Printf("web: admin not configured — first-time setup state")
	} else {
		s.logger.Printf("web: session authentication enabled (user: %s)", s.username)
	}

	addr := net.JoinHostPort("0.0.0.0", strconv.Itoa(port))
	readHeaderTimeout := s.cfg.ReadHeaderTimeout
	if readHeaderTimeout == 0 {
		readHeaderTimeout = 5 * time.Second
	}
	readTimeout := s.cfg.ReadTimeout
	if readTimeout == 0 {
		readTimeout = 10 * time.Second
	}
	writeTimeout := s.cfg.WriteTimeout
	if writeTimeout == 0 {
		writeTimeout = 0 // streaming endpoints (SSE/MSE) must not time out
	}
	idleTimeout := s.cfg.IdleTimeout
	if idleTimeout == 0 {
		idleTimeout = 120 * time.Second
	}
	s.server = &http.Server{
		Addr:              addr,
		Handler:           s.corsMiddleware(securityHeaders(s.observeMiddleware(s.mux))),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Printf("web: server starting on %s", addr)
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	go s.hub.runLog(ctx)

	select {
	case <-ctx.Done():
		return s.Stop()
	case err := <-errCh:
		return err
	}
}

// Stop stops the web server.
func (s *Server) Stop() error {
	if s.server == nil {
		return nil
	}
	return s.server.Close()
}

// registerRoutes registers all HTTP routes (SPEC v1).
func (s *Server) registerRoutes() {
	m := s.mux

	// Static assets (the shared mibee-webui build) — public.
	m.HandleFunc("GET /{$}", s.handleStatic)
	m.HandleFunc("GET /style.css", s.handleStatic)
	m.HandleFunc("GET /js/", s.handleStatic)

	// Public API (SPEC §1–2).
	m.HandleFunc("GET /api/health", s.handleHealth)
	m.HandleFunc("GET /api/auth/me", s.handleMe)
	m.HandleFunc("POST /api/auth/setup", s.handleSetup)
	m.HandleFunc("POST /api/auth/login", s.handleLogin)
	m.HandleFunc("POST /api/auth/logout", s.handleLogout)
	m.HandleFunc("POST /api/auth/reset", s.authRequired(s.handleReset))

	// Session-gated core (SPEC §3–5).
	m.HandleFunc("GET /api/status", s.authRequired(s.handleStatus))
	m.HandleFunc("GET /api/capabilities", s.authRequired(s.handleCapabilities))
	m.HandleFunc("GET /api/cameras", s.authRequired(s.handleCameras))
	m.HandleFunc("GET /api/cameras/{id}", s.authRequired(s.handleCameraGet))
	m.HandleFunc("GET /api/cameras/{id}/snapshot", s.authRequired(s.handleCameraSnapshot))
	m.HandleFunc("GET /api/cameras/{id}/stream.mse", s.authRequired(s.handleStreamMSEPath))
	m.HandleFunc("GET /api/config", s.authRequired(s.handleGetConfig))
	m.HandleFunc("PUT /api/config", s.authRequired(s.handlePutConfig))
	m.HandleFunc("POST /api/system/restart", s.authRequired(s.handleSystemRestart))

	// Imaging extension (SPEC §4.5).
	m.HandleFunc("GET /api/cameras/{id}/imaging/params", s.authRequired(s.handleGetCameraParams))
	m.HandleFunc("GET /api/cameras/{id}/imaging/options", s.authRequired(s.handleGetCameraOptions))
	m.HandleFunc("POST /api/cameras/{id}/imaging/param", s.authRequired(s.handlePostCameraParam))

	// AI detections (SPEC v1 §4.6).
	m.HandleFunc("GET /api/detections", s.authRequired(s.handleDetections))
	m.HandleFunc("GET /api/ai/models", s.authRequired(s.handleAIModels))
	m.HandleFunc("POST /api/ai/models/{id}/activate", s.authRequired(s.handleAIModelActivate))
	m.HandleFunc("POST /api/ai/models/{id}", s.authRequired(s.handleAIModelUpload))
	m.HandleFunc("DELETE /api/ai/models/{id}", s.authRequired(s.handleAIModelDelete))

	// SSE events (SPEC §6).
	m.HandleFunc("GET /api/events", s.authRequired(s.handleEvents))
	// Observability (SPEC §3.2)
	m.HandleFunc("GET /api/metrics/summary", s.authRequired(s.handleMetricsSummary))
	m.HandleFunc("GET /api/logs", s.authRequired(s.handleLogs))
	m.HandleFunc("GET /api/requests", s.authRequired(s.handleRequests))

	// Legacy snapshot for NVRs — byte-stable, deliberately open (dialect A2).
	if s.cfg.Metrics != nil {
		m.Handle("GET /metrics", s.cfg.Metrics)
	}
	if s.cfg.Snapshot != nil {
		m.HandleFunc("GET /snapshot", s.cfg.Snapshot.ServeHTTP)
	}
}

// Mux returns the server's ServeMux, allowing external packages to register
// routes (HLS mounts here). Nil until Start() is called.
func (s *Server) Mux() *http.ServeMux {
	return s.mux
}

// handleCameraGet serves GET /api/cameras/{id} (single camera, id "0").
func (s *Server) handleCameraGet(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != "0" {
		writeError(w, http.StatusNotFound, "no such camera")
		return
	}
	writeOK(w, http.StatusOK, s.cameraDoc())
}

// handleCameraSnapshot serves GET /api/cameras/{id}/snapshot (JPEG).
func (s *Server) handleCameraSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != "0" {
		writeError(w, http.StatusNotFound, "no such camera")
		return
	}
	if s.cfg.Snapshot == nil {
		http.Error(w, "snapshot not available", http.StatusServiceUnavailable)
		return
	}
	s.cfg.Snapshot.ServeHTTP(w, r)
}

// handleStreamMSEPath adapts the path-param route onto handleStreamMSE.
func (s *Server) handleStreamMSEPath(w http.ResponseWriter, r *http.Request) {
	s.handleStreamMSE(w, r, r.PathValue("id"))
}

// handleStatic serves the embedded shared frontend (index.html, style.css,
// js/*) with an SPA fallback.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "static/index.html"
	} else {
		path = "static/" + path
	}
	data, err := staticFS.ReadFile(path)
	if err != nil {
		// SPA fallback for unknown paths.
		if index, ierr := staticFS.ReadFile("static/index.html"); ierr == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			w.Write(index)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", mimeByExt(path))
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

func mimeByExt(path string) string {
	switch {
	case strings.HasSuffix(path, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(path, ".css"):
		return "text/css"
	case strings.HasSuffix(path, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(path, ".json"):
		return "application/json"
	case strings.HasSuffix(path, ".svg"):
		return "image/svg+xml"
	default:
		return "application/octet-stream"
	}
}

// persistWebCredentials writes the web section of the YAML config file so
// setup/reset survive a restart. Best-effort: memory credentials still apply.
func (s *Server) persistWebCredentials(username, password string) error {
	if s.cfg.ConfigPath == "" {
		return fmt.Errorf("config path not configured")
	}
	data, err := os.ReadFile(s.cfg.ConfigPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg map[string]interface{}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	cfg["web"] = map[string]interface{}{
		"enabled":  true,
		"port":     s.cfg.Port,
		"username": username,
		"password": password,
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return atomicWrite(s.cfg.ConfigPath, out)
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"ok":false,"error":"internal_error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(data)
	w.Write([]byte("\n"))
}

// securityHeaders sets security-related HTTP headers on all responses.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip sensitive credentials from URL query parameters.
		if q := r.URL.Query(); q.Has("password") || q.Has("username") {
			q.Del("username")
			q.Del("password")
			clean := r.URL.Path
			if enc := q.Encode(); enc != "" {
				clean += "?" + enc
			}
			http.Redirect(w, r, clean, http.StatusFound)
			return
		}

		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		// CSP only on HTML/static routes (no WebSocket anymore: connect-src
		// 'self' covers fetch + EventSource).
		path := r.URL.Path
		if path == "/" || strings.HasPrefix(path, "/js/") || path == "/style.css" {
			w.Header().Set("Content-Security-Policy",
				"default-src 'none'; "+
					"script-src 'self'; "+
					"style-src 'self' 'unsafe-inline'; "+
					"img-src 'self' data: blob:; "+
					"font-src 'self'; "+
					"connect-src 'self'; "+
					"media-src 'self' blob:; "+
					"base-uri 'self'; "+
					"form-action 'self'; "+
					"frame-ancestors 'none'")
		}

		next.ServeHTTP(w, r)
	})
}

// corsMiddleware restricts CORS access to allowed origins.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			allowed := false
			for _, o := range s.allowedOrigins {
				if o == "*" || o == origin {
					allowed = true
					break
				}
			}
			if allowed {
				if len(s.allowedOrigins) == 1 && s.allowedOrigins[0] == "*" {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				} else {
					w.Header().Set("Access-Control-Allow-Origin", origin)
				}
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-CSRF-Token")
			w.Header().Set("Access-Control-Allow-Credentials", "true")

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// maskPassword returns "****" if the password is non-empty, "" otherwise
// (SPEC §5).
func maskPassword(pw string) string {
	if pw == "" {
		return ""
	}
	return "****"
}

// coerceFloat64 converts JSON-decoded float64 to int for integer params.
func coerceFloat64(v interface{}) interface{} {
	f, ok := v.(float64)
	if !ok {
		return v
	}
	if f == float64(int(f)) && !strings.Contains(fmt.Sprintf("%g", f), ".") {
		return int(f)
	}
	return f
}

// Observe exposes the shared observability state (SPEC §3.2) so the
// process-wide sampler and log tee can be wired from main.
func (s *Server) Observe() *Observe { return s.observe }
