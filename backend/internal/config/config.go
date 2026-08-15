package config

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Config struct {
	System  System  `json:"system"`
	GRBL    GRBL    `json:"grbl"`
	Camera  Camera  `json:"camera"`
	SSH     SSH     `json:"ssh"`
	Network Network `json:"network"`
	WiFi    WiFi    `json:"wifi"`
}

type System struct {
	Hostname      string `json:"hostname"`
	SetupComplete bool   `json:"setup_complete"`
}

type GRBL struct {
	// Backend selects which service owns the serial port: the long-standing
	// ser2net, or the appliance's own laserbridged. Both ship permanently -
	// ser2net is the fallback, not a transitional measure (ADR 0011) - and
	// exactly one runs at a time, because two processes on one serial port is
	// the failure this bridge exists to prevent.
	Backend        string `json:"backend"`
	Device         string `json:"device"`
	Baudrate       int    `json:"baudrate"`
	Port           int    `json:"port"`
	MaxConnections int    `json:"max_connections"`
	Reconnect      bool   `json:"reconnect"`
	KickOldUser    bool   `json:"kick_old_user"`
	// OnDisconnect is what the bridge does when the client vanishes while the
	// machine is moving: "none", "hold" or "reset". This is a convenience, not
	// a safety device - see docs/adr/0010.
	OnDisconnect string `json:"on_disconnect"`
	// MonitorPort serves a read-only copy of the traffic for diagnosis. Zero
	// switches it off, which is the default: a port nobody asked for is a port
	// nobody is watching.
	MonitorPort int `json:"monitor_port"`
}

type Camera struct {
	Device     string `json:"device"`
	Format     string `json:"format"`
	Resolution string `json:"resolution"`
	FPS        int    `json:"fps"`
	Port       int    `json:"port"`
	Quality    int    `json:"quality"`
}

type SSH struct {
	Enabled                bool `json:"enabled"`
	PasswordAuthentication bool `json:"password_authentication"`
}

type Network struct {
	Mode    string   `json:"mode"`
	Address string   `json:"address,omitempty"`
	Gateway string   `json:"gateway,omitempty"`
	DNS     []string `json:"dns,omitempty"`
}

type WiFi struct {
	Enabled bool   `json:"enabled"`
	SSID    string `json:"ssid"`
	PSK     string `json:"-"`
	Country string `json:"country"`
	Hidden  bool   `json:"hidden"`
}

func Default() Config {
	return Config{
		System: System{Hostname: "laserbridge", SetupComplete: false},
		GRBL: GRBL{
			Backend: BackendSer2net,
			Device:  "/dev/ttyUSB0", Baudrate: 115200, Port: 23,
			MaxConnections: 1, Reconnect: true, KickOldUser: true,
			OnDisconnect: DisconnectHold,
		},
		Camera: Camera{
			Device: "/dev/video0", Format: "MJPEG", Resolution: "1280x720",
			FPS: 30, Port: 8080, Quality: 80,
		},
		// Password login is on by default: this appliance sits on an isolated
		// workshop network and is meant to be reachable without a key file
		// (ADR 0006). Key authentication stays available alongside it.
		SSH:     SSH{Enabled: true, PasswordAuthentication: true},
		Network: Network{Mode: "dhcp"},
		WiFi:    WiFi{Enabled: false, Country: "DE"},
	}
}

// The GRBL backends the appliance can run.
const (
	BackendSer2net      = "ser2net"
	BackendLaserbridged = "laserbridged"
)

// What the bridge does when a client disappears while the machine is moving.
const (
	// DisconnectNone forwards nothing. The machine finishes whatever is in its
	// planner buffer, as it did before the bridge could tell the difference.
	DisconnectNone = "none"
	// DisconnectHold sends a feed hold, which pauses motion and switches the
	// laser off while keeping the position - a job can be resumed afterwards.
	DisconnectHold = "hold"
	// DisconnectReset sends a feed hold and then a soft reset, which also
	// clears the planner buffer. The job is over, but nothing keeps moving.
	DisconnectReset = "reset"
)

var (
	hostnameRE   = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
	resolutionRE = regexp.MustCompile(`^[1-9][0-9]{1,4}x[1-9][0-9]{1,4}$`)
)

func (c Config) Validate() error {
	var problems []string
	if !hostnameRE.MatchString(c.System.Hostname) {
		problems = append(problems, "system.hostname must be a single RFC 1123 label")
	}
	if c.GRBL.Backend != BackendSer2net && c.GRBL.Backend != BackendLaserbridged {
		problems = append(problems, "grbl.backend must be ser2net or laserbridged")
	}
	if !validDevice(c.GRBL.Device, []string{"/dev/ttyUSB", "/dev/ttyACM", "/dev/serial/by-id/"}) {
		problems = append(problems, "grbl.device must be a supported absolute device path")
	}
	validBaud := map[int]bool{9600: true, 19200: true, 38400: true, 57600: true, 115200: true, 230400: true, 250000: true, 500000: true, 1000000: true}
	if !validBaud[c.GRBL.Baudrate] {
		problems = append(problems, "grbl.baudrate is unsupported")
	}
	if err := validatePort("grbl.port", c.GRBL.Port); err != nil {
		problems = append(problems, err.Error())
	}
	if c.GRBL.MaxConnections < 1 || c.GRBL.MaxConnections > 16 {
		problems = append(problems, "grbl.max_connections must be between 1 and 16")
	}
	switch c.GRBL.OnDisconnect {
	case DisconnectNone, DisconnectHold, DisconnectReset:
	default:
		problems = append(problems, "grbl.on_disconnect must be none, hold or reset")
	}
	if c.GRBL.MonitorPort != 0 {
		if err := validatePort("grbl.monitor_port", c.GRBL.MonitorPort); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if !validDevice(c.Camera.Device, []string{"/dev/video", "/dev/v4l/by-id/"}) {
		problems = append(problems, "camera.device must be a supported absolute device path")
	}
	if c.Camera.Format != "MJPEG" && c.Camera.Format != "YUYV" {
		problems = append(problems, "camera.format must be MJPEG or YUYV")
	}
	if !resolutionRE.MatchString(c.Camera.Resolution) {
		problems = append(problems, "camera.resolution must look like 1280x720")
	} else {
		parts := strings.Split(c.Camera.Resolution, "x")
		w, _ := strconv.Atoi(parts[0])
		h, _ := strconv.Atoi(parts[1])
		if w > 8192 || h > 8192 {
			problems = append(problems, "camera.resolution is too large")
		}
	}
	if c.Camera.FPS < 1 || c.Camera.FPS > 120 {
		problems = append(problems, "camera.fps must be between 1 and 120")
	}
	if err := validatePort("camera.port", c.Camera.Port); err != nil {
		problems = append(problems, err.Error())
	}
	if c.Camera.Quality < 1 || c.Camera.Quality > 100 {
		problems = append(problems, "camera.quality must be between 1 and 100")
	}
	// Every listening port has to be its own, including the optional monitor
	// port. Two services on one port means one of them silently does not start.
	taken := map[int]string{22: "SSH", 80: "the web interface"}
	for _, claim := range []struct {
		name string
		port int
	}{{"grbl.port", c.GRBL.Port}, {"camera.port", c.Camera.Port}, {"grbl.monitor_port", c.GRBL.MonitorPort}} {
		if claim.port == 0 {
			continue
		}
		if owner, clash := taken[claim.port]; clash {
			problems = append(problems, fmt.Sprintf("%s uses port %d, which belongs to %s", claim.name, claim.port, owner))
			continue
		}
		taken[claim.port] = claim.name
	}
	if c.Network.Mode != "dhcp" && c.Network.Mode != "static" {
		problems = append(problems, "network.mode must be dhcp or static")
	}
	if c.Network.Mode == "static" {
		if _, _, err := net.ParseCIDR(c.Network.Address); err != nil {
			problems = append(problems, "network.address must be CIDR notation")
		}
		if net.ParseIP(c.Network.Gateway) == nil {
			problems = append(problems, "network.gateway must be an IP address")
		}
	}
	for _, server := range c.Network.DNS {
		if net.ParseIP(server) == nil {
			problems = append(problems, "network.dns contains an invalid IP address")
		}
	}
	if len(c.WiFi.SSID) > 32 || strings.ContainsAny(c.WiFi.SSID, "\x00\n\r") {
		problems = append(problems, "wifi.ssid must be at most 32 bytes without control characters")
	}
	if c.WiFi.Enabled && c.WiFi.SSID == "" {
		problems = append(problems, "wifi.ssid is required when Wi-Fi is enabled")
	}
	if c.WiFi.Enabled && (len(c.WiFi.PSK) < 8 || len(c.WiFi.PSK) > 63 || strings.ContainsAny(c.WiFi.PSK, "\x00\n\r")) {
		problems = append(problems, "wifi.psk must contain 8 to 63 characters")
	}
	if !regexp.MustCompile(`^[A-Z]{2}$`).MatchString(c.WiFi.Country) {
		problems = append(problems, "wifi.country must be a two-letter uppercase country code")
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func validatePort(name string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s must be between 1 and 65535", name)
	}
	return nil
}

func validDevice(path string, prefixes []string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\n\r\x00") {
		return false
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(path, prefix) && len(path) > len(prefix) {
			return true
		}
	}
	return false
}
