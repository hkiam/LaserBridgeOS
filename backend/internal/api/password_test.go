package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
	lbruntime "github.com/laserbridgeos/laserbridgeos/backend/internal/runtime"
	lbupdate "github.com/laserbridgeos/laserbridgeos/backend/internal/update"
)

// cryptRunner stands in for busybox cryptpw. It reproduces just enough of
// crypt(3) for the tests: the same salt always yields the same hash, and a
// different password yields a different one.
type cryptRunner struct{}

func (cryptRunner) Run(name string, args ...string) ([]byte, error) { return nil, nil }

func (cryptRunner) RunWithInput(input, name string, args ...string) ([]byte, error) {
	if name != "cryptpw" {
		return nil, nil
	}
	salt := "generated"
	for i, arg := range args {
		if arg == "-S" && i+1 < len(args) {
			salt = args[i+1]
		}
	}
	password := strings.TrimSuffix(input, "\n")
	return []byte("$6$" + salt + "$hash-of-" + password + "\n"), nil
}

func passwordServer(t *testing.T, currentHash string) (http.Handler, *lbruntime.Manager) {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	store := config.NewStore(filepath.Join(dataDir, "config.yaml"))
	if _, err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg, _ := store.Load()
	cfg.System.SetupComplete = true
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	shadow := "root:!::0:::::\nlaserbridge:" + currentHash + ":20000:0:99999:7:::\n"
	if err := os.WriteFile(filepath.Join(dataDir, "shadow"), []byte(shadow), 0640); err != nil {
		t.Fatal(err)
	}
	manager := &lbruntime.Manager{Store: store, Dir: filepath.Join(dir, "run"), DataDir: dataDir, Run: cryptRunner{}}
	updater := &lbupdate.Manager{DataDir: dataDir, RuntimeDir: filepath.Join(dir, "run"), VersionPath: filepath.Join(dir, "version")}
	server := &Server{Store: store, Runtime: manager, Runner: cryptRunner{}, Updater: updater}
	return server.Handler(), manager
}

func changePassword(t *testing.T, handler http.Handler, current, next string) *httptest.ResponseRecorder {
	t.Helper()
	cookie, token := csrf(t, handler)
	body, _ := json.Marshal(passwordRequest{Current: current, New: next})
	request := httptest.NewRequest(http.MethodPut, "/api/system/password", bytes.NewReader(body))
	request.Header.Set("X-CSRF-Token", token)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestPasswordChangeRequiresTheCurrentOne(t *testing.T) {
	handler, manager := passwordServer(t, "$6$salt$hash-of-oldpassword")

	if code := changePassword(t, handler, "guessed", "newpassword").Code; code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a wrong current password", code)
	}
	hash, _ := manager.StoredPasswordHash()
	if hash != "$6$salt$hash-of-oldpassword" {
		t.Fatalf("password changed despite a wrong current one: %q", hash)
	}

	if code := changePassword(t, handler, "oldpassword", "newpassword").Code; code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for the right current password", code)
	}
	hash, _ = manager.StoredPasswordHash()
	if !strings.HasSuffix(hash, "hash-of-newpassword") {
		t.Fatalf("password not updated: %q", hash)
	}
}

func TestPasswordChangeRejectsWeakPassword(t *testing.T) {
	handler, _ := passwordServer(t, "$6$salt$hash-of-oldpassword")
	if code := changePassword(t, handler, "oldpassword", "short").Code; code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", code)
	}
}

func installUpdateRequest(t *testing.T, handler http.Handler, password string) *httptest.ResponseRecorder {
	t.Helper()
	cookie, token := csrf(t, handler)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if password != "" {
		if err := writer.WriteField("password", password); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("bundle", "update.lbu")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("not a real bundle")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/update/install", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-CSRF-Token", token)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestUpdateNeedsPasswordOrSignature(t *testing.T) {
	handler, _ := passwordServer(t, "$6$salt$hash-of-chosenpassword")

	response := installUpdateRequest(t, handler, "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without any authorisation: %s", response.Code, response.Body.String())
	}

	response = installUpdateRequest(t, handler, "wrongpassword")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "wrong password") {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}

	// The right password gets past authorisation; the bundle itself is
	// rubbish, so the install must still fail - but on its own merits.
	response = installUpdateRequest(t, handler, "chosenpassword")
	if response.Code == http.StatusUnauthorized {
		t.Fatalf("the correct password was not accepted: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "password") {
		t.Fatalf("expected a bundle error, got: %s", response.Body.String())
	}
}

func TestUpdateRefusesTheShippedDefaultPassword(t *testing.T) {
	handler, _ := passwordServer(t, lbruntime.DefaultPasswordHash)
	// The default is printed in the README, so it must not be enough to
	// authorise firmware even when the caller types it correctly.
	response := installUpdateRequest(t, handler, lbruntime.DefaultPassword)
	if !strings.Contains(response.Body.String(), "default password") {
		t.Fatalf("default password was not refused: %d %s", response.Code, response.Body.String())
	}
}
