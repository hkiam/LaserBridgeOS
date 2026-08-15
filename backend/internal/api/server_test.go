package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
	lbruntime "github.com/laserbridgeos/laserbridgeos/backend/internal/runtime"
)

type fakeRunner struct {
	mu    sync.Mutex
	calls []string
}

func (r *fakeRunner) Run(name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	return nil, nil
}

func testServer(t *testing.T) (http.Handler, *config.Store, *fakeRunner) {
	t.Helper()
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.yaml"))
	if _, err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	// One root for the whole server: nothing here may read - or write - the
	// machine running the test.
	root := filepath.Join(dir, "root")
	manager := &lbruntime.Manager{Store: store, Dir: filepath.Join(dir, "run"), DataDir: filepath.Join(dir, "data"), Run: runner, Root: root}
	return (&Server{Store: store, Runtime: manager, Runner: runner, VersionPath: filepath.Join(dir, "version"), Root: root}).Handler(), store, runner
}

func csrf(t *testing.T, handler http.Handler) (*http.Cookie, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("session response: %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Result().Cookies()[0], body["csrf_token"]
}

func TestConfigMutationNeedsCSRF(t *testing.T) {
	handler, store, _ := testServer(t)
	cfg, _ := store.Load()
	body, _ := json.Marshal(cfg)
	req := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestConfigMutationValidatesAndRestarts(t *testing.T) {
	handler, store, runner := testServer(t)
	cookie, token := csrf(t, handler)
	cfg, _ := store.Load()
	cfg.GRBL.Port = 2323
	body, _ := json.Marshal(cfg)
	req := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body))
	req.Host = "laserbridge.local"
	req.Header.Set("Origin", "http://laserbridge.local")
	req.Header.Set("X-CSRF-Token", token)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	loaded, _ := store.Load()
	if loaded.GRBL.Port != 2323 {
		t.Fatalf("saved port = %d", loaded.GRBL.Port)
	}
	joined := strings.Join(runner.calls, "\n")
	if !strings.Contains(joined, "rc-service ser2net restart") {
		t.Fatalf("runner calls lack restart: %s", joined)
	}
}

func TestConfigMutationRejectsUnknownJSON(t *testing.T) {
	handler, _, _ := testServer(t)
	cookie, token := csrf(t, handler)
	req := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(`{"surprise":true}`))
	req.Header.Set("X-CSRF-Token", token)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestServiceAllowlist(t *testing.T) {
	handler, _, _ := testServer(t)
	cookie, token := csrf(t, handler)
	req := httptest.NewRequest(http.MethodPost, "/api/services/not-a-service/restart", nil)
	req.Header.Set("X-CSRF-Token", token)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
