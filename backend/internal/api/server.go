package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
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
	Store       *config.Store
	Runtime     *lbruntime.Manager
	Runner      lbruntime.Runner
	Updater     *lbupdate.Manager
	WebRoot     string
	VersionPath string
	// BridgeSocket is where laserbridged answers; empty means the default.
	BridgeSocket string
	// BridgeWatch answers whether the daemon is still responsive. Nil where
	// nothing is watching, which is the read-only case and the tests.
	BridgeWatch *lbruntime.BridgeWatch
	// Root is where this server reads the running system: /proc for load,
	// memory and sockets, /sys for the wireless interface and the camera's
	// name, /dev for the devices themselves. Empty means "/", which is the
	// appliance; a test points it at a directory it built.
	//
	// One field, not one per subsystem. The status page reports on the machine
	// it runs on, and a test that can only redirect half of those paths is a
	// test that reads the developer's machine for the other half.
	Root   string
	Logger *log.Logger
	// passwords slows down guessing of the one secret this appliance has,
	// shared by every endpoint that checks it.
	passwords attemptGuard
	mu        sync.Mutex
	updateMu  sync.Mutex
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
	mux.HandleFunc("GET /api/grbl", s.grblStatus)
	mux.HandleFunc("GET /api/grbl/journal", s.grblJournal)
	mux.HandleFunc("GET /api/config", s.getConfig)
	mux.HandleFunc("PUT /api/config", s.mutation(s.putConfig))
	mux.HandleFunc("POST /api/services/{service}/{action}", s.mutation(s.serviceAction))
	mux.HandleFunc("POST /api/system/reboot", s.mutation(s.reboot))
	mux.HandleFunc("PUT /api/system/password", s.mutation(s.changePassword))
	mux.HandleFunc("GET /api/logs", s.logs)
	for _, path := range []string{"/generate_204", "/hotspot-detect.html", "/connecttest.txt", "/ncsi.txt"} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		})
	}
	if s.WebRoot != "" {
		mux.Handle("/", s.staticFiles())
	}
	return s.securityHeaders(mux)
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' http:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")
		// Nothing the API says is worth keeping a copy of. The status changes
		// by the second, and the requests that carry the appliance password
		// cross an ordinary HTTP connection - they must not also come to rest
		// in a cache along the way.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
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
	w.Header().Set("ETag", configETag(cfg))
	writeJSON(w, http.StatusOK, cfg)
}

// configETag names a particular configuration, so a writer can say which one it
// was editing.
//
// The whole document is sent on every save, which means two people - two
// browser tabs, a second operator, a page left open since yesterday while
// somebody edited /data/config.yaml over SSH - silently overwrite each other,
// and the loser never learns that anything happened. The mutex serialises the
// writes; it cannot tell that one of them was composed against a version that
// no longer exists.
//
// It is the marshalled form that is hashed rather than the struct: that is
// exactly what is stored, so two configurations with the same tag are the same
// file, and a tag survives a round trip through the parser unchanged.
func configETag(cfg config.Config) string {
	data, err := config.MarshalYAML(cfg)
	if err != nil {
		// An unmarshallable configuration cannot be named, and must not be
		// given a name that another one could accidentally match.
		return ""
	}
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
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
	// A caller that says which configuration it was editing is told when that
	// is no longer the one on disk. Saying nothing is still allowed - a shell
	// script with curl has no page to have loaded - so this protects the
	// interface that does send it rather than everybody equally.
	if tag := r.Header.Get("If-Match"); tag != "" && tag != configETag(previous) {
		writeError(w, http.StatusPreconditionFailed,
			"the configuration changed somewhere else since this page loaded; reload it before saving")
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
	// Saving GRBL settings restarts the bridge, which drops the client. Other
	// sections do not touch it, so only this one has to ask.
	if previous.GRBL != next.GRBL && s.refuseWhileBusy(w, r, "not restarting the bridge") {
		return
	}
	// Handing the kernel permission to suspend an idle USB device reaches the
	// adapter that is carrying the job. Taking that permission away cannot cost
	// anything, so only the direction that adds a way to lose bytes has to ask.
	if next.System.USBAutosuspend >= 0 && next.System.USBAutosuspend != previous.System.USBAutosuspend &&
		s.refuseWhileBusy(w, r, "not allowing the kernel to suspend USB devices") {
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
	// The name of what was just written, so the page can keep editing without
	// another round trip.
	w.Header().Set("ETag", configETag(next))
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
		// Both GRBL backends are told to reconsider. Whichever the
		// configuration now names starts; the other stops, because two
		// processes on one serial port is the failure to avoid.
		result = append(result, "ser2net", "laserbridged")
	}
	if a.Camera != b.Camera {
		result = append(result, "ustreamer")
	}
	if a.SSH != b.SSH {
		result = append(result, "sshd")
	}
	// Only the hostname: avahi advertises that and nothing else in this
	// section, and restarting it for a USB power setting would drop every
	// browser's mDNS name for the appliance to no purpose.
	if a.System.Hostname != b.System.Hostname {
		result = append(result, "avahi-daemon")
	}
	// Wi-Fi is deliberately absent: putConfig carries the previous Wi-Fi
	// settings over unchanged, so only PUT /api/wifi can alter them and that
	// handler restarts the network itself.
	return result
}

func desiredAction(service string, cfg config.Config) string {
	switch service {
	case "sshd":
		if !cfg.SSH.Enabled {
			return "stop"
		}
	case "ser2net", "laserbridged":
		if cfg.GRBL.Backend != service {
			return "stop"
		}
	}
	return "restart"
}

func (s *Server) serviceAction(w http.ResponseWriter, r *http.Request) {
	service := r.PathValue("service")
	action := r.PathValue("action")
	allowedService := map[string]bool{"ser2net": true, "laserbridged": true, "ustreamer": true, "sshd": true, "avahi-daemon": true, "laserbridge-web": true, "laserbridge-watchdog": true}
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

func (s *Server) reboot(w http.ResponseWriter, r *http.Request) {
	if s.Runner == nil {
		writeError(w, http.StatusServiceUnavailable, "reboot is unavailable")
		return
	}
	if s.refuseWhileBusy(w, r, "not rebooting") {
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
	// The log is a file on /data so that it survives the reboot that follows
	// whatever went wrong. logread stays as the fallback for an appliance whose
	// syslogd was pointed back at the shared-memory ring, and for the moment
	// after a fresh boot when the file does not exist yet.
	if data, err := os.ReadFile(s.dataPath("log", "messages")); err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": tail(string(data), limit)})
		return
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
	writeJSON(w, http.StatusOK, map[string]any{"lines": tail(string(out), limit)})
}

func tail(text string, limit int) []string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines
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

// system is the filesystem the appliance reads facts about itself from. It is
// a value rather than a scattering of literals so that the whole status page
// can be pointed at a directory a test built - and so that no test can read, or
// write, the machine running it by accident.
type system string

func (r system) path(parts ...string) string {
	return filepath.Join(append([]string{string(r)}, parts...)...)
}

func (s *Server) system() system { return system(s.Root) }

func (r system) exists(path string) bool {
	_, err := os.Stat(r.path(path))
	return err == nil
}

func readTrimmed(path string) string {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
