package api

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type wifiNetwork struct {
	SSID      string  `json:"ssid"`
	Signal    float64 `json:"signal_dbm"`
	Frequency int     `json:"frequency_mhz"`
	Security  string  `json:"security"`
}

type serialDevice struct {
	Path   string `json:"path"`
	Stable bool   `json:"stable"`
}

func (s *Server) wifiScan(w http.ResponseWriter, _ *http.Request) {
	if s.Runner == nil {
		writeError(w, http.StatusServiceUnavailable, "Wi-Fi scanning is unavailable")
		return
	}
	interfaceName := s.wirelessInterface()
	if interfaceName == "" {
		writeError(w, http.StatusNotFound, "no Wi-Fi interface found")
		return
	}
	output, err := s.Runner.Run("iw", "dev", interfaceName, "scan")
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if len(detail) > 240 {
			detail = detail[:240]
		}
		if detail == "" {
			detail = "the adapter may not support scanning while the setup AP is active"
		}
		writeError(w, http.StatusServiceUnavailable, "Wi-Fi scan unavailable: "+detail)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interface": interfaceName, "networks": parseIWScan(output)})
}

func (s *Server) wirelessInterface() string {
	root := s.system().path("/sys/class/net")
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			if info, err := os.Stat(filepath.Join(root, entry.Name(), "wireless")); err == nil && info.IsDir() {
				return entry.Name()
			}
		}
	}
	return ""
}

func parseIWScan(output []byte) []wifiNetwork {
	var current *wifiNetwork
	privacy := false
	security := "Open"
	bySSID := map[string]wifiNetwork{}
	flush := func() {
		if current == nil || current.SSID == "" {
			return
		}
		current.Security = security
		if security == "Open" && privacy {
			current.Security = "Secured"
		}
		if previous, exists := bySSID[current.SSID]; !exists || current.Signal > previous.Signal {
			bySSID[current.SSID] = *current
		}
	}
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "BSS ") {
			flush()
			current = &wifiNetwork{Signal: -999}
			privacy = false
			security = "Open"
			continue
		}
		if current == nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "SSID: "):
			current.SSID = decodeIWSSID(strings.TrimPrefix(line, "SSID: "))
		case strings.HasPrefix(line, "signal: "):
			value := strings.Fields(strings.TrimPrefix(line, "signal: "))
			if len(value) > 0 {
				current.Signal, _ = strconv.ParseFloat(value[0], 64)
			}
		case strings.HasPrefix(line, "freq: "):
			current.Frequency, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "freq: ")))
		case strings.HasPrefix(line, "capability:") && strings.Contains(line, "Privacy"):
			privacy = true
		case line == "WPA:":
			security = "WPA"
		case line == "RSN:":
			security = "WPA2/WPA3"
		case strings.Contains(line, "Authentication suites:") && strings.Contains(line, "SAE"):
			security = "WPA3"
		}
	}
	flush()
	result := make([]wifiNetwork, 0, len(bySSID))
	for _, network := range bySSID {
		result = append(result, network)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Signal == result[j].Signal {
			return result[i].SSID < result[j].SSID
		}
		return result[i].Signal > result[j].Signal
	})
	return result
}

func decodeIWSSID(value string) string {
	var result strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] == '\\' && index+3 < len(value) && value[index+1] == 'x' {
			decoded, err := strconv.ParseUint(value[index+2:index+4], 16, 8)
			if err == nil {
				result.WriteByte(byte(decoded))
				index += 3
				continue
			}
		}
		result.WriteByte(value[index])
	}
	if !utf8.ValidString(result.String()) {
		return fmt.Sprintf("invalid-ssid-%x", []byte(result.String()))
	}
	return result.String()
}

type videoDevice struct {
	Path         string `json:"path"`
	Stable       bool   `json:"stable"`
	Name         string `json:"name"`
	Capabilities string `json:"capabilities"`
}

func (s *Server) devices(w http.ResponseWriter, _ *http.Request) {
	sys := s.system()
	serial := sys.discover([]string{"/dev/serial/by-id/*", "/dev/ttyUSB*", "/dev/ttyACM*"})
	video := sys.discover([]string{"/dev/v4l/by-id/*", "/dev/video*"})
	serialResult := make([]serialDevice, 0, len(serial))
	for _, path := range serial {
		serialResult = append(serialResult, serialDevice{Path: path, Stable: strings.Contains(path, "/by-id/")})
	}
	videoResult := make([]videoDevice, 0, len(video))
	for _, path := range video {
		// Resolved where it was found, named where it will be used.
		name := readTrimmed(sys.path("/sys/class/video4linux", filepath.Base(resolve(sys.path(path))), "name"))
		capabilities := ""
		if s.Runner != nil {
			out, err := s.Runner.Run("v4l2-ctl", "--device", path, "--list-formats-ext")
			capabilities = strings.TrimSpace(string(out))
			// A node that answers with an empty format list cannot be streamed
			// from. Modern kernels register a metadata node next to every
			// capture node, and offering it as a camera only invites a broken
			// selection. A failed query says nothing about the device - it may
			// simply be busy - so that device is kept.
			if err == nil && capabilities == "" {
				continue
			}
		}
		videoResult = append(videoResult, videoDevice{Path: path, Stable: strings.Contains(path, "/by-id/"), Name: name, Capabilities: capabilities})
	}
	writeJSON(w, http.StatusOK, map[string]any{"serial": serialResult, "video": videoResult})
}

// discover lists the device nodes matching patterns, in pattern order. A
// device reachable under several names - the stable /dev/*/by-id/ symlink and
// the kernel's own /dev/video0 - is reported once, under the first pattern
// that found it, so that querying it stays a single call.
//
// The names it reports are the ones the appliance uses, not the ones it looked
// under: with a root pointing somewhere else the paths are searched there but
// still named /dev/..., because that is what goes into the configuration and
// what the bridge will open.
func (r system) discover(patterns []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(r.path(pattern))
		sort.Strings(matches)
		for _, match := range matches {
			target := resolve(match)
			if _, err := os.Stat(match); err == nil && !seen[target] {
				seen[target] = true
				result = append(result, r.unrooted(match))
			}
		}
	}
	return result
}

// unrooted turns a path that was looked up under Root back into the name the
// appliance knows it by.
func (r system) unrooted(path string) string {
	if r == "" {
		return path
	}
	trimmed := strings.TrimPrefix(path, string(r))
	if !strings.HasPrefix(trimmed, "/") {
		return "/" + trimmed
	}
	return trimmed
}

func resolve(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}
