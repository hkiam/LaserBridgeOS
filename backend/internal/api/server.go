package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
	lbruntime "github.com/laserbridgeos/laserbridgeos/backend/internal/runtime"
	lbupdate "github.com/laserbridgeos/laserbridgeos/backend/internal/update"
)

const csrfCookie = "laserbridge_csrf"

type Server struct {
	Store         *config.Store
	Runtime       *lbruntime.Manager
	Runner        lbruntime.Runner
	Updater       *lbupdate.Manager
	WebRoot       string
	VersionPath   string
	WirelessSysfs string
	Logger        *log.Logger
	mu            sync.Mutex
	updateMu      sync.Mutex
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/session", s.session)
	mux.HandleFunc("GET /api/setup", s.setupStatus)
	mux.HandleFunc("POST /api/setup/ssh-key", s.mutation(s.setupSSHKey))
	mux.HandleFunc("POST /api/setup/complete", s.mutation(s.completeSetup))
	mux.HandleFunc("PUT /api/wifi", s.mutation(s.updateWiFi))
	mux.HandleFunc("GET /api/wifi/scan", s.wifiScan)
	mux.HandleFunc("GET /api/update/status", s.updateStatus)
	mux.HandleFunc("POST /api/update/install", s.mutation(s.installUpdate))
	mux.HandleFunc("POST /api/update/rollback", s.mutation(s.rollbackUpdate))
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/devices", s.devices)
	mux.HandleFunc("GET /api/config", s.getConfig)
	mux.HandleFunc("PUT /api/config", s.mutation(s.putConfig))
	mux.HandleFunc("POST /api/services/{service}/{action}", s.mutation(s.serviceAction))
	mux.HandleFunc("POST /api/system/reboot", s.mutation(s.reboot))
	mux.HandleFunc("GET /api/logs", s.logs)
	for _, path := range []string{"/generate_204", "/hotspot-detect.html", "/connecttest.txt", "/ncsi.txt"} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		})
	}
	if s.WebRoot != "" {
		mux.Handle("/", http.FileServer(http.Dir(s.WebRoot)))
	}
	return s.securityHeaders(mux)
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' http:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) mutation(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		cookie, err := r.Cookie(csrfCookie)
		header := r.Header.Get("X-CSRF-Token")
		if err != nil || header == "" || len(cookie.Value) != len(header) || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(header)) != 1 {
			writeError(w, http.StatusForbidden, "invalid CSRF token")
			return
		}
		next(w, r)
	}
}

func sameOrigin(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host) && (u.Scheme == "http" || u.Scheme == "https")
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	if existing, err := r.Cookie(csrfCookie); err == nil && len(existing.Value) >= 32 {
		writeJSON(w, http.StatusOK, map[string]string{"csrf_token": existing.Value})
		return
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		writeError(w, http.StatusInternalServerError, "could not create session")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(random)
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
	writeJSON(w, http.StatusOK, map[string]string{"csrf_token": token})
}

func (s *Server) getConfig(w http.ResponseWriter, _ *http.Request) {
	cfg, err := s.Store.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var next config.Config
	if err := decoder.Decode(&next); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := ensureEOF(decoder); err != nil {
		writeError(w, http.StatusBadRequest, "request must contain one JSON object")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, err := s.Store.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Secrets and first-boot state cannot be overwritten through the general
	// configuration document returned to the browser.
	next.System.SetupComplete = previous.System.SetupComplete
	next.WiFi = previous.WiFi
	if err := next.Validate(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := s.Store.Save(next); err != nil {
		writeError(w, http.StatusInternalServerError, "save failed: "+err.Error())
		return
	}
	if s.Runtime != nil {
		if err := s.Runtime.Apply(); err != nil {
			_ = s.Store.Save(previous)
			_ = s.Runtime.Apply()
			writeError(w, http.StatusInternalServerError, "apply failed; previous config restored: "+err.Error())
			return
		}
	}
	var warnings []string
	if s.Runner != nil {
		for _, service := range changedServices(previous, next) {
			if out, err := s.Runner.Run("rc-service", service, desiredAction(service, next)); err != nil {
				warnings = append(warnings, fmt.Sprintf("%s: %s", service, strings.TrimSpace(string(out))))
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": next, "warnings": warnings})
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return errors.New("extra JSON value")
}

func changedServices(a, b config.Config) []string {
	var result []string
	if a.GRBL != b.GRBL {
		result = append(result, "ser2net")
	}
	if a.Camera != b.Camera {
		result = append(result, "ustreamer")
	}
	if a.SSH != b.SSH {
		result = append(result, "sshd")
	}
	if a.System != b.System {
		result = append(result, "avahi-daemon")
	}
	if a.WiFi != b.WiFi {
		result = append(result, "laserbridge-network")
	}
	return result
}

func desiredAction(service string, cfg config.Config) string {
	if service == "sshd" && !cfg.SSH.Enabled {
		return "stop"
	}
	return "restart"
}

func (s *Server) serviceAction(w http.ResponseWriter, r *http.Request) {
	service := r.PathValue("service")
	action := r.PathValue("action")
	allowedService := map[string]bool{"ser2net": true, "ustreamer": true, "sshd": true, "avahi-daemon": true, "laserbridge-web": true}
	allowedAction := map[string]bool{"start": true, "stop": true, "restart": true}
	if !allowedService[service] || !allowedAction[action] {
		writeError(w, http.StatusNotFound, "unknown service action")
		return
	}
	if s.Runner == nil {
		writeError(w, http.StatusServiceUnavailable, "service control is unavailable")
		return
	}
	out, err := s.Runner.Run("rc-service", service, action)
	if err != nil {
		writeError(w, http.StatusInternalServerError, strings.TrimSpace(string(out)))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": service, "action": action})
}

func (s *Server) reboot(w http.ResponseWriter, _ *http.Request) {
	if s.Runner == nil {
		writeError(w, http.StatusServiceUnavailable, "reboot is unavailable")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "rebooting"})
	go func() {
		time.Sleep(500 * time.Millisecond)
		if out, err := s.Runner.Run("reboot"); err != nil && s.Logger != nil {
			s.Logger.Printf("reboot failed: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}()
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 1 && parsed <= 1000 {
			limit = parsed
		}
	}
	if s.Runner == nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}})
		return
	}
	out, err := s.Runner.Run("logread")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read logs")
		return
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func uniqueAddresses() []string {
	var result []string
	seen := map[string]bool{}
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil && ip.IsGlobalUnicast() && !seen[ip.String()] {
				seen[ip.String()] = true
				result = append(result, ip.String())
			}
		}
	}
	return result
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readTrimmed(path string) string {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
