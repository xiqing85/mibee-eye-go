package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// SPEC v1 §2 session auth: cookie session + CSRF double-submit.
//
// - `session` cookie: HttpOnly, SameSite=Strict, 24h. The store persists to
//   disk when a path is configured (NewSessionStoreAt) so the deliberate
//   self-restarts (config save, imaging flips, /api/system/restart) keep
//   browsers signed in (附录A #10); logout/reset still clear it.
// - `csrf-token` cookie: readable by JS; every state-changing /api request
//   must echo it in the X-CSRF-Token header (login/setup/logout exempt).
// - First boot (no password configured): /api/auth/me answers 503
//   setup_required and POST /api/auth/setup creates the admin.

const (
	sessionTTL  = 24 * time.Hour
	cleanupTick = 1 * time.Hour

	sessionCookie = "session"
	csrfCookie    = "csrf-token"
	csrfHeader    = "X-CSRF-Token"
)

// loginRateLimiter tracks failed login attempts per IP.
type loginRateLimiter struct {
	mu       sync.Mutex
	attempts map[string]*rateLimitEntry
}

type rateLimitEntry struct {
	count        int
	windowStart  time.Time
	blockedUntil time.Time
}

const (
	maxLoginAttempts   = 10
	loginWindow        = 1 * time.Minute
	loginBlockDuration = 5 * time.Minute
)

// session represents an authenticated user session.
type session struct {
	username  string
	csrf      string
	createdAt time.Time
	expiresAt time.Time
}

// SessionStore manages cookie sessions. In-memory by default; with a path
// (NewSessionStoreAt) it round-trips them to disk so self-restarts keep
// sessions alive.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]session
	path     string
}

// NewSessionStore creates an empty in-memory session store. Credentials
// live on the Server (they may change via setup/reset); an empty password
// simply means first-boot state.
func NewSessionStore() *SessionStore {
	s := &SessionStore{sessions: make(map[string]session)}
	go s.cleanup()
	return s
}

// sessionRecord is the on-disk shape of a session (web-sessions.json).
type sessionRecord struct {
	Username  string    `json:"username"`
	CSRF      string    `json:"csrf"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type sessionsDoc struct {
	Version  int                       `json:"version"`
	Sessions map[string]sessionRecord `json:"sessions"`
}

// NewSessionStoreAt loads (or creates) a session store backed by path.
// Expired entries are pruned at load; a missing or corrupt file degrades
// to an empty store — sessions are a cache, never worth failing startup.
func NewSessionStoreAt(path string) *SessionStore {
	s := &SessionStore{sessions: make(map[string]session), path: path}
	data, err := os.ReadFile(path)
	if err == nil {
		var doc sessionsDoc
		if json.Unmarshal(data, &doc) == nil {
			now := time.Now()
			for token, rec := range doc.Sessions {
				if now.After(rec.ExpiresAt) {
					continue
				}
				s.sessions[token] = session{
					username:  rec.Username,
					csrf:      rec.CSRF,
					createdAt: rec.CreatedAt,
					expiresAt: rec.ExpiresAt,
				}
			}
		}
	}
	go s.cleanup()
	return s
}

// persistLocked snapshots the store to disk (best effort). Caller holds
// the write lock. atomicWrite creates the temp file 0600, so the file is
// never group/world-readable.
func (s *SessionStore) persistLocked() {
	if s.path == "" {
		return
	}
	doc := sessionsDoc{Version: 1, Sessions: make(map[string]sessionRecord, len(s.sessions))}
	for token, sess := range s.sessions {
		doc.Sessions[token] = sessionRecord{
			Username:  sess.username,
			CSRF:      sess.csrf,
			CreatedAt: sess.createdAt,
			ExpiresAt: sess.expiresAt,
		}
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return
	}
	_ = atomicWrite(s.path, data)
}

func newToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Create issues a session, returning (session token, csrf token).
func (s *SessionStore) Create(username string) (string, string, error) {
	token, err := newToken()
	if err != nil {
		return "", "", err
	}
	csrf, err := newToken()
	if err != nil {
		return "", "", err
	}
	now := time.Now()
	s.mu.Lock()
	s.sessions[token] = session{
		username:  username,
		csrf:      csrf,
		createdAt: now,
		expiresAt: now.Add(sessionTTL),
	}
	s.persistLocked()
	s.mu.Unlock()
	return token, csrf, nil
}

// Validate checks a session token, returning (username, csrf) or an error.
func (s *SessionStore) Validate(token string) (string, string, error) {
	if token == "" {
		return "", "", errors.New("missing token")
	}
	s.mu.RLock()
	sess, ok := s.sessions[token]
	s.mu.RUnlock()
	if !ok {
		return "", "", errors.New("invalid token")
	}
	if time.Now().After(sess.expiresAt) {
		s.mu.Lock()
		delete(s.sessions, token)
		s.mu.Unlock()
		return "", "", errors.New("token expired")
	}
	return sess.username, sess.csrf, nil
}

// Logout invalidates a session. No-op if the token is the empty string.
func (s *SessionStore) Logout(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, token)
	s.persistLocked()
	s.mu.Unlock()
}

// Clear invalidates every session (password reset).
func (s *SessionStore) Clear() {
	s.mu.Lock()
	s.sessions = make(map[string]session)
	s.persistLocked()
	s.mu.Unlock()
}

// Count returns the number of active sessions (for diagnostics).
func (s *SessionStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// cleanup periodically prunes expired sessions.
func (s *SessionStore) cleanup() {
	t := time.NewTicker(cleanupTick)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		s.mu.Lock()
		for token, sess := range s.sessions {
			if now.After(sess.expiresAt) {
				delete(s.sessions, token)
			}
		}
		s.mu.Unlock()
	}
}

// allow checks if the given IP is allowed to attempt login.
func (l *loginRateLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.attempts[ip]
	if !ok {
		return true
	}
	now := time.Now()
	if now.Before(entry.blockedUntil) {
		return false
	}
	if !entry.blockedUntil.IsZero() {
		delete(l.attempts, ip)
		return true
	}
	if now.Sub(entry.windowStart) > loginWindow {
		delete(l.attempts, ip)
		return true
	}
	return entry.count < maxLoginAttempts
}

// recordFailure increments the failed attempt counter for the given IP.
func (l *loginRateLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	entry, ok := l.attempts[ip]
	if !ok {
		l.attempts[ip] = &rateLimitEntry{count: 1, windowStart: now}
		return
	}
	if now.Before(entry.blockedUntil) {
		return
	}
	if now.Sub(entry.windowStart) > loginWindow {
		entry.count = 1
		entry.windowStart = now
		entry.blockedUntil = time.Time{}
		return
	}
	entry.count++
	if entry.count >= maxLoginAttempts {
		entry.blockedUntil = now.Add(loginBlockDuration)
	}
}

// recordSuccess resets the rate limiter for the given IP.
func (l *loginRateLimiter) recordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, ip)
}

// extractIP returns the client IP address from the request, stripping the port.
func extractIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// sessionCookieValue reads the `session` cookie from the request.
func sessionCookieValue(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// setSessionCookies issues the session + csrf cookies on the response.
func setSessionCookies(w http.ResponseWriter, token, csrf string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    csrf,
		Path:     "/",
		SameSite: http.SameSiteStrictMode,
	})
}

// isWrite reports whether the method changes state (CSRF applies).
func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		return true
	}
	return false
}

// csrfExempt paths carry no session yet (SPEC §2).
func csrfExempt(path string) bool {
	switch path {
	case "/api/auth/login", "/api/auth/setup", "/api/auth/logout":
		return true
	}
	return false
}

// authRequired wraps a handler with session-cookie validation plus the CSRF
// double-submit check on writes.
func (s *Server) authRequired(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := sessionCookieValue(r)
		_, csrf, err := s.sessions.Validate(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		if isWriteMethod(r.Method) && !csrfExempt(r.URL.Path) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get(csrfHeader)), []byte(csrf)) != 1 {
				writeError(w, http.StatusUnauthorized, "csrf mismatch")
				return
			}
		}
		next(w, r)
	}
}

// credentialsEqual compares a login attempt in constant time.
func (s *Server) credentialsEqual(user, pass string) bool {
	userMatch := subtle.ConstantTimeCompare([]byte(user), []byte(s.username)) == 1
	passMatch := subtle.ConstantTimeCompare([]byte(pass), []byte(s.password)) == 1
	return userMatch && passMatch
}

// handleMe answers the UI's auth-state probe (SPEC §2): 200 signed in,
// 401 login, 503 setup_required.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if !s.configured() {
		writeError(w, http.StatusServiceUnavailable, "setup_required")
		return
	}
	if username, _, err := s.sessions.Validate(sessionCookieValue(r)); err == nil {
		writeOK(w, http.StatusOK, map[string]interface{}{"username": username, "role": "admin"})
		return
	}
	writeError(w, http.StatusUnauthorized, "not signed in")
}

// handleSetup creates the admin account on first boot (SPEC §2).
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if s.configured() {
		writeError(w, http.StatusBadRequest, "already configured")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Username == "" || len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "username required and password >= 8 chars")
		return
	}

	s.mu.Lock()
	s.username = req.Username
	s.password = req.Password
	s.mu.Unlock()

	if err := s.persistWebCredentials(req.Username, req.Password); err != nil {
		slog.Warn("web: setup could not persist credentials", "err", err)
	}

	token, csrf, err := s.sessions.Create(req.Username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session error")
		return
	}
	setSessionCookies(w, token, csrf)
	slog.Info("web: admin created", "user", req.Username)
	writeOK(w, http.StatusOK, map[string]interface{}{"username": req.Username})
}

// handleLogin establishes a session (SPEC §2). Accepts any username while
// the stored one is the ONVIF fallback (migration dialect).
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.configured() {
		writeError(w, http.StatusServiceUnavailable, "setup_required")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// SPEC §2: empty/omitted username defaults to the configured admin
	// account (single-admin login form; ONVIF fallback may set another name).
	if req.Username == "" {
		s.mu.RLock()
		if s.username != "" {
			req.Username = s.username
		} else {
			req.Username = "admin"
		}
		s.mu.RUnlock()
	}
	if req.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}

	ip := extractIP(r)
	if !s.loginLimiter.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "too many login attempts, try again later")
		return
	}

	if !s.credentialsEqual(req.Username, req.Password) {
		s.loginLimiter.recordFailure(ip)
		slog.Warn("web: login failed", "user", req.Username)
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	s.loginLimiter.recordSuccess(ip)
	token, csrf, err := s.sessions.Create(s.username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session error")
		return
	}
	slog.Info("web: login OK", "user", s.username, "active_sessions", s.sessions.Count())
	setSessionCookies(w, token, csrf)
	writeOK(w, http.StatusOK, map[string]interface{}{"username": s.username})
}

// handleLogout drops the session and clears the cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := sessionCookieValue(r); token != "" {
		if username, _, err := s.sessions.Validate(token); err == nil {
			s.sessions.Logout(token)
			slog.Info("web: logout OK", "user", username, "active_sessions", s.sessions.Count())
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

// handleReset changes the admin password, invalidating every session.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.NewPassword) < 8 {
		writeError(w, http.StatusBadRequest, "password >= 8 chars")
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.OldPassword), []byte(s.password)) != 1 {
		writeError(w, http.StatusUnauthorized, "wrong password")
		return
	}

	s.mu.Lock()
	s.password = req.NewPassword
	s.mu.Unlock()
	if err := s.persistWebCredentials(s.username, req.NewPassword); err != nil {
		slog.Warn("web: reset could not persist credentials", "err", err)
	}
	s.sessions.Clear()

	token, csrf, err := s.sessions.Create(s.username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session error")
		return
	}
	setSessionCookies(w, token, csrf)
	writeOK(w, http.StatusOK, map[string]interface{}{"username": s.username})
}

// configured reports whether an admin credential exists (web section or the
// ONVIF fallback).
func (s *Server) configured() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.password != ""
}

// currentCredentials returns the resolved credentials.
func (s *Server) currentCredentials() (string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.username, s.password
}

var _ = strings.TrimSpace // keep strings import if unused paths change
