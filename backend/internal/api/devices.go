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
	root := s.WirelessSysfs
	if root == "" {
		root = "/sys/class/net"
	}
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
	serial := discover([]string{"/dev/serial/by-id/*", "/dev/ttyUSB*", "/dev/ttyACM*"})
	video := discover([]string{"/dev/v4l/by-id/*", "/dev/video*"})
	serialResult := make([]serialDevice, 0, len(serial))
	for _, path := range serial {
		serialResult = append(serialResult, serialDevice{Path: path, Stable: strings.Contains(path, "/by-id/")})
	}
	videoResult := make([]videoDevice, 0, len(video))
	for _, path := range video {
		resolved, _ := filepath.EvalSymlinks(path)
		base := filepath.Base(resolved)
		name := readTrimmed(filepath.Join("/sys/class/video4linux", base, "name"))
		capabilities := ""
		if s.Runner != nil {
			if out, err := s.Runner.Run("v4l2-ctl", "--device", path, "--list-formats-ext"); err == nil {
				capabilities = strings.TrimSpace(string(out))
			}
		}
		videoResult = append(videoResult, videoDevice{Path: path, Stable: strings.Contains(path, "/by-id/"), Name: name, Capabilities: capabilities})
	}
	writeJSON(w, http.StatusOK, map[string]any{"serial": serialResult, "video": videoResult})
}

func discover(patterns []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		sort.Strings(matches)
		for _, match := range matches {
			if _, err := os.Stat(match); err == nil && !seen[match] {
				seen[match] = true
				result = append(result, match)
			}
		}
	}
	return result
}
