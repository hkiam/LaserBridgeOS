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
	// One root for the whole server: nothing here may read - or write - the
	// machine running the test.
	root := filepath.Join(dir, "root")
	manager := &lbruntime.Manager{Store: store, Dir: filepath.Join(dir, "run"), DataDir: dataDir, Root: root}
	server := &Server{Store: store, Runtime: manager, Root: root}
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

type passwordRunner struct {
	inputs []string
	calls  []string
}

func (r *passwordRunner) Run(name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, name)
	return nil, nil
}

func (r *passwordRunner) RunWithInput(input, name string, args ...string) ([]byte, error) {
	r.inputs = append(r.inputs, input)
	r.calls = append(r.calls, name)
	if name == "cryptpw" {
		return []byte("$6$fakesalt$fakehashvalue\n"), nil
	}
	return nil, nil
}

func TestSetupWithPasswordNeedsNoKeyFile(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	store := config.NewStore(filepath.Join(dataDir, "config.yaml"))
	if _, err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	runner := &passwordRunner{}
	// One root for the whole server: nothing here may read - or write - the
	// machine running the test.
	root := filepath.Join(dir, "root")
	manager := &lbruntime.Manager{Store: store, Dir: filepath.Join(dir, "run"), DataDir: dataDir, Run: runner, Root: root}
	handler := (&Server{Store: store, Runtime: manager, Runner: runner, Root: root}).Handler()

	// A key that a previous step authorized must survive a password-only setup.
	existing := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEXISTING earlier@key"
	if err := os.MkdirAll(filepath.Join(dataDir, "ssh"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "ssh", "authorized_keys"), []byte(existing+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// /etc/shadow is a symlink into /data on the appliance; the account
	// database has to be there for a password change to land anywhere.
	shadow := filepath.Join(dataDir, "shadow")
	if err := os.WriteFile(shadow, []byte("root:!::0:::::\nlaserbridge:$6$old$hash:20000:0:99999:7:::\n"), 0640); err != nil {
		t.Fatal(err)
	}

	cookie, token := csrf(t, handler)
	body, _ := json.Marshal(setupRequest{
		Hostname: "workshop-laser", SSID: "Workshop WiFi", PSK: "safe-password",
		Country: "de", Password: "workshop-secret",
	})
	request := httptest.NewRequest(http.MethodPost, "/api/setup/complete", bytes.NewReader(body))
	request.Header.Set("X-CSRF-Token", token)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}

	// The password must reach the hashing tool on stdin, never as an argument
	// where the target's process list would expose it.
	if len(runner.inputs) != 1 || runner.inputs[0] != "workshop-secret\n" {
		t.Fatalf("password was not passed on stdin: %q", runner.inputs)
	}
	updated, err := os.ReadFile(shadow)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(updated, []byte("laserbridge:$6$fakesalt$fakehashvalue:")) {
		t.Fatalf("shadow entry not updated:\n%s", updated)
	}
	if !bytes.Contains(updated, []byte("root:!::")) {
		t.Fatalf("shadow lost its other entries:\n%s", updated)
	}
	authorized, err := os.ReadFile(filepath.Join(dataDir, "ssh", "authorized_keys"))
	if err != nil || string(authorized) != existing+"\n" {
		t.Fatalf("authorized_keys changed during a password-only setup: %q, %v", authorized, err)
	}
	cfg, _ := store.Load()
	if !cfg.System.SetupComplete || !cfg.SSH.PasswordAuthentication {
		t.Fatalf("unexpected config after setup: %#v", cfg.System)
	}
}

func TestSetupRejectsShortPassword(t *testing.T) {
	handler, _, _ := setupTestServer(t)
	cookie, token := csrf(t, handler)
	body, _ := json.Marshal(setupRequest{
		Hostname: "workshop-laser", SSID: "Workshop WiFi", PSK: "safe-password",
		Country: "de", Password: "short",
	})
	request := httptest.NewRequest(http.MethodPost, "/api/setup/complete", bytes.NewReader(body))
	request.Header.Set("X-CSRF-Token", token)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", response.Code, response.Body.String())
	}
}

func TestSetupKeepsTheDeploymentKeyOfARAMSession(t *testing.T) {
	handler, _, dataDir := setupTestServer(t)
	setupDir := filepath.Join(dataDir, "setup")
	if err := os.MkdirAll(setupDir, 0700); err != nil {
		t.Fatal(err)
	}
	deploymentKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDEPLOY operator@mac"
	injected := filepath.Join(dataDir, "ramboot-authorized-keys")
	if err := os.WriteFile(injected, []byte(deploymentKey+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	previous := rambootKeyPath
	rambootKeyPath = injected
	t.Cleanup(func() { rambootKeyPath = previous })

	cookie, token := csrf(t, handler)
	body, _ := json.Marshal(setupRequest{
		Hostname: "workshop-laser", SSID: "Workshop WiFi", PSK: "safe-password",
		Country: "de", PublicKey: testPublicKey(),
	})
	request := httptest.NewRequest(http.MethodPost, "/api/setup/complete", bytes.NewReader(body))
	request.Header.Set("X-CSRF-Token", token)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}

	authorized, err := os.ReadFile(filepath.Join(dataDir, "ssh", "authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	// Running the wizard inside a RAM session must not lock out the machine
	// that started it: both the chosen key and the deployment key stay.
	for _, want := range []string{testPublicKey(), deploymentKey} {
		if !bytes.Contains(authorized, []byte(want)) {
			t.Fatalf("authorized_keys lost %q:\n%s", want, authorized)
		}
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
