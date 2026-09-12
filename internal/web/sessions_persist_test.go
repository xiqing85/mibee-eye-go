package web

// Sessions must survive the service's deliberate self-restarts (config
// save, imaging flips, POST /api/system/restart all SIGTERM the process —
// SPEC 附录A #10). An in-memory-only store logs every browser out on each
// flip, which reads as "the page kicked me to login" — the store persists
// to disk and reloads on startup instead.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A session created in one store is valid in a store reloaded from the
// same path (process restart simulation).
func TestSessionsSurviveStoreReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web-sessions.json")
	a := NewSessionStoreAt(path)

	token, csrf, err := a.Create("admin")
	if err != nil {
		t.Fatal(err)
	}

	b := NewSessionStoreAt(path)
	user, gotCSRF, err := b.Validate(token)
	if err != nil {
		t.Fatalf("session must survive reload: %v", err)
	}
	if user != "admin" || gotCSRF != csrf {
		t.Fatalf("reloaded session mismatch: %q %q", user, gotCSRF)
	}
}

// Logout and Clear write through to disk — a restart must not resurrect
// signed-out sessions.
func TestLogoutAndClearPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web-sessions.json")
	a := NewSessionStoreAt(path)
	tok, _, _ := a.Create("admin")
	a.Logout(tok)
	if _, _, err := NewSessionStoreAt(path).Validate(tok); err == nil {
		t.Fatal("logged-out session must not resurrect after reload")
	}

	a.Clear()
	b, _, _ := a.Create("admin")
	a.Clear()
	if _, _, err := NewSessionStoreAt(path).Validate(b); err == nil {
		t.Fatal("cleared sessions must not resurrect after reload")
	}
}

// Expired sessions are skipped at load; a corrupt file degrades to an
// empty store instead of breaking startup.
func TestLoadPrunesExpiredAndToleratesCorruptFile(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "web-sessions.json")
	a := NewSessionStoreAt(path)
	tok, _, _ := a.Create("admin")
	// Backdate the session past its TTL directly in the file.
	raw, _ := os.ReadFile(path)
	var doc struct {
		Sessions map[string]sessionRecord `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	rec := doc.Sessions[tok]
	rec.ExpiresAt = time.Now().Add(-time.Hour)
	doc.Sessions[tok] = rec
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewSessionStoreAt(path).Validate(tok); err == nil {
		t.Fatal("expired session must not load")
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	os.WriteFile(corrupt, []byte("{not json"), 0o600)
	st := NewSessionStoreAt(corrupt)
	if _, _, err := st.Validate("anything"); err == nil {
		t.Fatal("corrupt file must degrade to an empty store")
	}
	if st.Count() != 0 {
		t.Fatalf("corrupt file must load empty, got %d sessions", st.Count())
	}
}

// The persisted file must not be world-readable (it holds session tokens).
func TestSessionFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web-sessions.json")
	NewSessionStoreAt(path).Create("admin")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("session file must be owner-only, got %v", info.Mode().Perm())
	}
}
