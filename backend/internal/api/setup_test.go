package api

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
	lbruntime "github.com/laserbridgeos/laserbridgeos/backend/internal/runtime"
)

func testPublicKey() string {
	name := []byte("ssh-ed25519")
	blob := make([]byte, 4+len(name)+4+32)
	binary.BigEndian.PutUint32(blob[:4], uint32(len(name)))
	copy(blob[4:], name)
	binary.BigEndian.PutUint32(blob[4+len(name):], 32)
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " test"
}

func setupTestServer(t *testing.T) (http.Handler, *config.Store, string) {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	store := config.NewStore(filepath.Join(dataDir, "config.yaml"))
	if _, err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	manager := &lbruntime.Manager{Store: store, Dir: filepath.Join(dir, "run"), DataDir: dataDir}
	server := &Server{Store: store, Runtime: manager}
	return server.Handler(), store, dataDir
}

func TestFirstBootSetupInstallsKeyAndWiFi(t *testing.T) {
	handler, store, dataDir := setupTestServer(t)
	setupDir := filepath.Join(dataDir, "setup")
	if err := os.MkdirAll(setupDir, 0700); err != nil {
		t.Fatal(err)
	}
	privatePath := filepath.Join(setupDir, "laserbridge_ed25519")
	if err := os.WriteFile(privatePath, []byte("private-key"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privatePath+".pub", []byte(testPublicKey()+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cookie, token := csrf(t, handler)
	body, _ := json.Marshal(setupRequest{
		Hostname: "Workshop-Laser", SSID: "Workshop WiFi", PSK: "safe-password",
		Country: "de", UseGeneratedKey: true,
	})
	request := httptest.NewRequest(http.MethodPost, "/api/setup/complete", bytes.NewReader(body))
	request.Header.Set("X-CSRF-Token", token)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}

	cfg, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.System.SetupComplete || cfg.System.Hostname != "workshop-laser" || !cfg.WiFi.Enabled || cfg.WiFi.PSK != "safe-password" {
		t.Fatalf("unexpected saved setup: %#v", cfg)
	}
	authorized, err := os.ReadFile(filepath.Join(dataDir, "ssh", "authorized_keys"))
	if err != nil || string(authorized) != testPublicKey()+"\n" {
		t.Fatalf("authorized key = %q, err = %v", authorized, err)
	}
	authorizedInfo, _ := os.Stat(filepath.Join(dataDir, "ssh", "authorized_keys"))
	if authorizedInfo.Mode().Perm() != 0644 {
		t.Fatalf("authorized_keys mode = %o", authorizedInfo.Mode().Perm())
	}
	if _, err := os.Stat(privatePath); !os.IsNotExist(err) {
		t.Fatalf("initial private key remains after setup: %v", err)
	}
	mode, _ := os.ReadFile(filepath.Join(filepath.Dir(store.Path()), "..", "run", "network-mode"))
	if string(mode) != "client\n" {
		t.Fatalf("network mode = %q", mode)
	}
}

func TestInitialKeyDownloadStopsAfterSetup(t *testing.T) {
	handler, store, dataDir := setupTestServer(t)
	keyPath := filepath.Join(dataDir, "setup", "laserbridge_ed25519")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	cookie, token := csrf(t, handler)
	request := httptest.NewRequest(http.MethodPost, "/api/setup/ssh-key", nil)
	request.Header.Set("X-CSRF-Token", token)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "secret" {
		t.Fatalf("download response = %d %q", response.Code, response.Body.String())
	}
	cfg, _ := store.Load()
	cfg.System.SetupComplete = true
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusGone {
		t.Fatalf("completed setup key status = %d", response.Code)
	}
}

func TestPublicKeyPayloadMustMatchType(t *testing.T) {
	if err := validatePublicKey("ssh-rsa " + string(bytes.Fields([]byte(testPublicKey()))[1])); err == nil {
		t.Fatal("mismatched key type was accepted")
	}
}

func TestConfigAPIKeepsWiFiPasswordSecret(t *testing.T) {
	handler, store, _ := setupTestServer(t)
	cfg, _ := store.Load()
	cfg.WiFi = config.WiFi{Enabled: true, SSID: "Workshop", PSK: "do-not-leak", Country: "DE"}
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || bytes.Contains(response.Body.Bytes(), []byte("do-not-leak")) || bytes.Contains(response.Body.Bytes(), []byte(`"psk"`)) {
		t.Fatalf("unsafe config response: %d %s", response.Code, response.Body.String())
	}
}
