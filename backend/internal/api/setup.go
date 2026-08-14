package api

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/atomicfile"
	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
	"github.com/laserbridgeos/laserbridgeos/backend/internal/runtime"
)

type setupRequest struct {
	Hostname        string `json:"hostname"`
	SSID            string `json:"ssid"`
	PSK             string `json:"psk"`
	Country         string `json:"country"`
	Hidden          bool   `json:"hidden"`
	UseGeneratedKey bool   `json:"use_generated_key"`
	PublicKey       string `json:"public_key"`
	// Password replaces the shipped default. Empty means keep whatever is
	// currently set, which on a fresh appliance is the documented default.
	Password string `json:"password"`
}

func defaultPasswordIfUnchanged(cfg config.Config) string {
	if cfg.System.SetupComplete {
		return ""
	}
	return runtime.DefaultPassword
}

// MinPasswordLength is deliberately modest. The appliance lives on an
// isolated workshop network and its own web interface has no login at all,
// so demanding a long passphrase here would buy nothing and cost usability.
const MinPasswordLength = 8

func validatePassword(value string) error {
	if len(value) < MinPasswordLength {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	if len(value) > 128 {
		return errors.New("password must be at most 128 characters")
	}
	if strings.ContainsAny(value, "\n\r\x00:") {
		return errors.New("password must not contain colons or line breaks")
	}
	return nil
}

type wifiRequest struct {
	Enabled bool   `json:"enabled"`
	SSID    string `json:"ssid"`
	PSK     string `json:"psk"`
	Country string `json:"country"`
	Hidden  bool   `json:"hidden"`
}

func (s *Server) dataPath(parts ...string) string {
	if s.Runtime != nil {
		return s.Runtime.DataPath(parts...)
	}
	return filepath.Join(append([]string{"/data"}, parts...)...)
}

func (s *Server) setupStatus(w http.ResponseWriter, _ *http.Request) {
	cfg, err := s.Store.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, keyErr := os.Stat(s.dataPath("setup", "laserbridge_ed25519"))
	writeJSON(w, http.StatusOK, map[string]any{
		"required":                !cfg.System.SetupComplete,
		"ap_ssid":                 readTrimmed(s.dataPath("setup", "ap_ssid")),
		"ap_password":             runtime.SetupAPPassword,
		"generated_key_available": keyErr == nil,
		"ssh_user":                "laserbridge",
		"hostname":                cfg.System.Hostname,
		"country":                 cfg.WiFi.Country,
		"password_login":          cfg.SSH.PasswordAuthentication,
		"min_password_length":     MinPasswordLength,
		// Only disclosed while the appliance is still unconfigured, and only
		// because it is printed in the README anyway.
		"default_password": defaultPasswordIfUnchanged(cfg),
	})
}

func (s *Server) setupSSHKey(w http.ResponseWriter, _ *http.Request) {
	cfg, err := s.Store.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cfg.System.SetupComplete {
		writeError(w, http.StatusGone, "initial SSH key is no longer available")
		return
	}
	key, err := os.ReadFile(s.dataPath("setup", "laserbridge_ed25519"))
	if err != nil || len(key) > 32<<10 {
		writeError(w, http.StatusNotFound, "initial SSH key is unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="laserbridge_ed25519"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(key)
}

func (s *Server) completeSetup(w http.ResponseWriter, r *http.Request) {
	var request setupRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	previous, err := s.Store.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if previous.System.SetupComplete {
		writeError(w, http.StatusConflict, "initial setup is already complete")
		return
	}
	if request.UseGeneratedKey && strings.TrimSpace(request.PublicKey) != "" {
		writeError(w, http.StatusUnprocessableEntity, "choose one SSH key option, not both")
		return
	}
	// An SSH key is optional. Password login is enabled by default, so a
	// setup that only changes the password is a complete and valid setup.
	publicKey := strings.TrimSpace(request.PublicKey)
	if request.UseGeneratedKey {
		data, readErr := os.ReadFile(s.dataPath("setup", "laserbridge_ed25519.pub"))
		if readErr != nil {
			writeError(w, http.StatusUnprocessableEntity, "generated SSH key is unavailable")
			return
		}
		publicKey = strings.TrimSpace(string(data))
	}
	if publicKey != "" {
		if err := validatePublicKey(publicKey); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	if request.Password != "" {
		if err := validatePassword(request.Password); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	if publicKey == "" && request.Password == "" && !previous.SSH.PasswordAuthentication {
		writeError(w, http.StatusUnprocessableEntity,
			"set a password or an SSH key, otherwise nothing could log in")
		return
	}

	next := previous
	next.System.Hostname = strings.ToLower(strings.TrimSpace(request.Hostname))
	next.System.SetupComplete = true
	next.WiFi = config.WiFi{Enabled: true, SSID: request.SSID, PSK: request.PSK, Country: strings.ToUpper(request.Country), Hidden: request.Hidden}
	if err := next.Validate(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// The password is changed first: it is the one step that cannot be undone
	// afterwards, so failing here leaves the appliance exactly as it was.
	if request.Password != "" {
		if s.Runtime == nil {
			writeError(w, http.StatusServiceUnavailable, "password changes are unavailable")
			return
		}
		if err := s.Runtime.SetPassword(request.Password); err != nil {
			if s.Logger != nil {
				s.Logger.Printf("set password: %v", err)
			}
			writeError(w, http.StatusInternalServerError, "could not set the password")
			return
		}
	}
	authorizedPath := s.dataPath("ssh", "authorized_keys")
	restoreAuthorized := authorizedRestorer(authorizedPath)
	// Only touch authorized_keys when a key was actually chosen; a
	// password-only setup must not discard the keys already installed.
	if publicKey != "" {
		if err := atomicfile.Write(authorizedPath, authorizedKeys(publicKey), 0644); err != nil {
			writeError(w, http.StatusInternalServerError, "could not install SSH key")
			return
		}
	}
	if err := s.Store.Save(next); err != nil {
		restoreAuthorized()
		writeError(w, http.StatusInternalServerError, "could not save setup")
		return
	}
	if s.Runtime != nil {
		if err := s.Runtime.Apply(); err != nil {
			_ = s.Store.Save(previous)
			restoreAuthorized()
			_ = s.Runtime.Apply()
			writeError(w, http.StatusInternalServerError, "could not apply setup; previous configuration restored")
			return
		}
	}
	// The generated private key is only worth keeping while it is the way in.
	// A setup that chose a password or brought its own key never needs it.
	_ = os.Remove(s.dataPath("setup", "laserbridge_ed25519"))
	sshCommand := fmt.Sprintf("ssh laserbridge@%s.local", next.System.Hostname)
	if request.UseGeneratedKey {
		sshCommand = fmt.Sprintf("ssh -i laserbridge_ed25519 laserbridge@%s.local", next.System.Hostname)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":   "switching-to-wifi",
		"hostname": next.System.Hostname + ".local",
		"ssh":      sshCommand,
	})
	s.restartNetworkSoon()
}

func (s *Server) updateWiFi(w http.ResponseWriter, r *http.Request) {
	var request wifiRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, err := s.Store.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	next := previous
	if request.PSK == "" && request.SSID == previous.WiFi.SSID {
		request.PSK = previous.WiFi.PSK
	}
	next.WiFi = config.WiFi{Enabled: request.Enabled, SSID: request.SSID, PSK: request.PSK, Country: strings.ToUpper(request.Country), Hidden: request.Hidden}
	if err := next.Validate(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := s.Store.Save(next); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.Runtime != nil {
		if err := s.Runtime.Apply(); err != nil {
			_ = s.Store.Save(previous)
			_ = s.Runtime.Apply()
			writeError(w, http.StatusInternalServerError, "could not apply Wi-Fi settings")
			return
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "switching-network"})
	s.restartNetworkSoon()
}

func (s *Server) restartNetworkSoon() {
	if s.Runner == nil {
		return
	}
	go func() {
		time.Sleep(750 * time.Millisecond)
		_, _ = s.Runner.Run("rc-service", "laserbridge-network", "restart")
		_, _ = s.Runner.Run("rc-service", "avahi-daemon", "restart")
	}()
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return errors.New("request must contain one JSON object")
	}
	return nil
}

func validatePublicKey(value string) error {
	if len(value) > 16<<10 || strings.ContainsAny(value, "\r\n") {
		return errors.New("SSH public key must be a single line")
	}
	fields := strings.Fields(value)
	if len(fields) < 2 {
		return errors.New("SSH public key is invalid")
	}
	allowed := map[string]bool{"ssh-ed25519": true, "ecdsa-sha2-nistp256": true, "ssh-rsa": true}
	if !allowed[fields[0]] {
		return errors.New("SSH key type must be Ed25519, ECDSA P-256, or RSA")
	}
	decoded, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(decoded) < 16 {
		return errors.New("SSH public key payload is invalid")
	}
	nameLength := int(binary.BigEndian.Uint32(decoded[:4]))
	if nameLength < 1 || 4+nameLength > len(decoded) || string(decoded[4:4+nameLength]) != fields[0] {
		return errors.New("SSH public key type does not match its payload")
	}
	return nil
}

// rambootKeyPath holds the key that deploy.sh injected into a RAM session.
// It is a package variable so tests can point it somewhere writable.
var rambootKeyPath = "/etc/laserbridge/ramboot-authorized-keys"

// authorizedKeys builds the authorized_keys content for a completed setup.
//
// Running the setup wizard replaces the file, which is right on an installed
// appliance. In a RAM session it would also discard the deployment key that
// put the system there, stranding the operator halfway through a deployment -
// so that key is kept alongside the one just chosen.
func authorizedKeys(publicKey string) []byte {
	content := publicKey + "\n"
	injected, err := os.ReadFile(rambootKeyPath)
	if err != nil {
		return []byte(content)
	}
	for _, line := range strings.Split(string(injected), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && line != strings.TrimSpace(publicKey) {
			content += line + "\n"
		}
	}
	return []byte(content)
}

// authorizedRestorer captures the current authorized_keys file so a failed
// setup can be undone. A file that did not exist yet is removed again rather
// than left behind empty, which would otherwise look like a deliberate
// "no key is authorized" state.
func authorizedRestorer(path string) func() {
	previous, err := os.ReadFile(path)
	if err != nil {
		return func() { _ = os.Remove(path) }
	}
	return func() { _ = atomicfile.Write(path, previous, 0644) }
}
