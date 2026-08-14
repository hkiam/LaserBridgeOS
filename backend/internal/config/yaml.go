package config

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// ParseYAML accepts the deliberately small YAML subset emitted by MarshalYAML.
// Keeping the appliance schema flat within named sections avoids a YAML runtime
// dependency while still making the on-disk file pleasant to edit by hand.
func ParseYAML(data []byte) (Config, error) {
	c := Default()
	section := ""
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		raw := strings.TrimRight(scanner.Text(), " \t\r")
		if strings.TrimSpace(raw) == "" || strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}
		if !strings.HasPrefix(raw, " ") {
			if !strings.HasSuffix(raw, ":") || strings.Contains(raw[:len(raw)-1], ":") {
				return Config{}, fmt.Errorf("line %d: expected section", lineNo)
			}
			section = strings.TrimSuffix(raw, ":")
			switch section {
			case "system", "grbl", "camera", "ssh", "network", "wifi":
			default:
				return Config{}, fmt.Errorf("line %d: unknown section %q", lineNo, section)
			}
			continue
		}
		if !strings.HasPrefix(raw, "  ") || strings.HasPrefix(raw, "   ") || section == "" {
			return Config{}, fmt.Errorf("line %d: expected a two-space-indented key", lineNo)
		}
		parts := strings.SplitN(strings.TrimSpace(raw), ":", 2)
		if len(parts) != 2 {
			return Config{}, fmt.Errorf("line %d: expected key: value", lineNo)
		}
		key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if err := setValue(&c, section, key, value); err != nil {
			return Config{}, fmt.Errorf("line %d: %w", lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return Config{}, err
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func setValue(c *Config, section, key, raw string) error {
	stringValue := func() (string, error) {
		if strings.HasPrefix(raw, `"`) {
			return strconv.Unquote(raw)
		}
		if strings.Contains(raw, " #") {
			raw = strings.SplitN(raw, " #", 2)[0]
		}
		return strings.TrimSpace(raw), nil
	}
	intValue := func() (int, error) { return strconv.Atoi(raw) }
	boolValue := func() (bool, error) { return strconv.ParseBool(raw) }

	var err error
	switch section + "." + key {
	case "system.hostname":
		c.System.Hostname, err = stringValue()
	case "system.setup_complete":
		c.System.SetupComplete, err = boolValue()
	case "grbl.device":
		c.GRBL.Device, err = stringValue()
	case "grbl.baudrate":
		c.GRBL.Baudrate, err = intValue()
	case "grbl.port":
		c.GRBL.Port, err = intValue()
	case "grbl.max_connections":
		c.GRBL.MaxConnections, err = intValue()
	case "grbl.reconnect":
		c.GRBL.Reconnect, err = boolValue()
	case "grbl.kick_old_user":
		c.GRBL.KickOldUser, err = boolValue()
	case "camera.device":
		c.Camera.Device, err = stringValue()
	case "camera.format":
		c.Camera.Format, err = stringValue()
	case "camera.resolution":
		c.Camera.Resolution, err = stringValue()
	case "camera.fps":
		c.Camera.FPS, err = intValue()
	case "camera.port":
		c.Camera.Port, err = intValue()
	case "camera.quality":
		c.Camera.Quality, err = intValue()
	case "ssh.enabled":
		c.SSH.Enabled, err = boolValue()
	case "ssh.password_authentication":
		c.SSH.PasswordAuthentication, err = boolValue()
	case "network.mode":
		c.Network.Mode, err = stringValue()
	case "network.address":
		c.Network.Address, err = stringValue()
	case "network.gateway":
		c.Network.Gateway, err = stringValue()
	case "network.dns":
		var value string
		value, err = stringValue()
		if err == nil && value != "" {
			for _, item := range strings.Split(value, ",") {
				c.Network.DNS = append(c.Network.DNS, strings.TrimSpace(item))
			}
		}
	case "wifi.enabled":
		c.WiFi.Enabled, err = boolValue()
	case "wifi.ssid":
		c.WiFi.SSID, err = stringValue()
	case "wifi.psk":
		c.WiFi.PSK, err = stringValue()
	case "wifi.country":
		c.WiFi.Country, err = stringValue()
	case "wifi.hidden":
		c.WiFi.Hidden, err = boolValue()
	default:
		return fmt.Errorf("unknown key %s.%s", section, key)
	}
	if err != nil {
		return fmt.Errorf("invalid value for %s.%s: %v", section, key, err)
	}
	return nil
}

func MarshalYAML(c Config) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	q := strconv.Quote
	var b strings.Builder
	fmt.Fprintf(&b, "system:\n  hostname: %s\n  setup_complete: %t\n", q(c.System.Hostname), c.System.SetupComplete)
	fmt.Fprintf(&b, "grbl:\n  device: %s\n  baudrate: %d\n  port: %d\n  max_connections: %d\n  reconnect: %t\n  kick_old_user: %t\n",
		q(c.GRBL.Device), c.GRBL.Baudrate, c.GRBL.Port, c.GRBL.MaxConnections, c.GRBL.Reconnect, c.GRBL.KickOldUser)
	fmt.Fprintf(&b, "camera:\n  device: %s\n  format: %s\n  resolution: %s\n  fps: %d\n  port: %d\n  quality: %d\n",
		q(c.Camera.Device), q(c.Camera.Format), q(c.Camera.Resolution), c.Camera.FPS, c.Camera.Port, c.Camera.Quality)
	fmt.Fprintf(&b, "ssh:\n  enabled: %t\n  password_authentication: %t\n", c.SSH.Enabled, c.SSH.PasswordAuthentication)
	fmt.Fprintf(&b, "network:\n  mode: %s\n  address: %s\n  gateway: %s\n  dns: %s\n",
		q(c.Network.Mode), q(c.Network.Address), q(c.Network.Gateway), q(strings.Join(c.Network.DNS, ",")))
	fmt.Fprintf(&b, "wifi:\n  enabled: %t\n  ssid: %s\n  psk: %s\n  country: %s\n  hidden: %t\n",
		c.WiFi.Enabled, q(c.WiFi.SSID), q(c.WiFi.PSK), q(c.WiFi.Country), c.WiFi.Hidden)
	return []byte(b.String()), nil
}
