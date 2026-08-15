package config

import (
	"bufio"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// errUnknownKey separates "this version has never heard of that setting" -
// which is what a configuration written by a newer release looks like - from a
// setting it knows and cannot read.
var errUnknownKey = errors.New("unknown key")

// ParseYAML accepts the deliberately small YAML subset emitted by MarshalYAML.
// Keeping the appliance schema flat within named sections avoids a YAML runtime
// dependency while still making the on-disk file pleasant to edit by hand.
//
// It also returns what it did not understand, having read the rest.
//
// That tolerance is the whole point, and it is aimed at one scenario: the A/B
// rollback. `/data` is shared by both slots, so an appliance that boots the
// previous slot after a bad update hands an older binary a file a newer one
// wrote. Rejecting the key that newer version added would fail the parse, and a
// failed parse used to mean the configuration was replaced by the defaults -
// which forgets the Wi-Fi credentials and puts the appliance back into
// first-boot access-point mode. The recovery path would take the appliance off
// the network at exactly the moment it is needed.
//
// So strictness sits where it belongs, and only there: a request arriving at
// the API with an unknown field is a caller with a bug and is refused, while a
// file on disk containing something this version has not heard of is a file
// written by a version that knew more. The shape stays part of the contract -
// sections of flat `key: value` scalars, two-space indented - because tolerating
// unknown *names* is forward compatibility, and tolerating unknown *structure*
// would be indistinguishable from tolerating damage.
func ParseYAML(data []byte) (Config, []string, error) {
	c, notes, err := parseYAML(data)
	if err != nil {
		return Config{}, nil, err
	}
	if err := c.Validate(); err != nil {
		return Config{}, nil, err
	}
	return c, notes, nil
}

// parseYAML reads the file without judging the result. Store.Ensure uses it to
// salvage what it can from a configuration that does not validate; everyone
// else wants ParseYAML, which validates.
func parseYAML(data []byte) (Config, []string, error) {
	c := Default()
	var notes []string
	// unknownSection marks a section this version has never heard of, so its
	// keys are skipped without a complaint each.
	const unknownSection = "\x00unknown"
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
				return Config{}, nil, fmt.Errorf("line %d: expected section", lineNo)
			}
			section = strings.TrimSuffix(raw, ":")
			switch section {
			case "system", "grbl", "camera", "ssh", "network", "wifi":
			default:
				notes = append(notes, fmt.Sprintf("section %q is not known to this version and was ignored", section))
				section = unknownSection
			}
			continue
		}
		if !strings.HasPrefix(raw, "  ") || strings.HasPrefix(raw, "   ") || section == "" {
			return Config{}, nil, fmt.Errorf("line %d: expected a two-space-indented key", lineNo)
		}
		parts := strings.SplitN(strings.TrimSpace(raw), ":", 2)
		if len(parts) != 2 {
			return Config{}, nil, fmt.Errorf("line %d: expected key: value", lineNo)
		}
		if section == unknownSection {
			continue
		}
		key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		// Every branch of setValue assigns before it can check, because that is
		// what "field, err = parse()" does in Go - so a value that will not
		// parse has already overwritten the default with a zero. Keeping a copy
		// is cheaper than making thirty assignments careful, and it makes the
		// promise below true rather than nearly true.
		before := c
		if err := setValue(&c, section, key, value); err != nil {
			c = before
			if errors.Is(err, errUnknownKey) {
				notes = append(notes, fmt.Sprintf("line %d: %v; a newer version may have written it", lineNo, err))
				continue
			}
			// The field keeps the default it already holds. One setting this
			// version cannot make sense of must not cost the other twenty -
			// a value whose range or spelling a later release widened reaches
			// an older one exactly this way.
			notes = append(notes, fmt.Sprintf("line %d: %v; the default was used instead", lineNo, err))
		}
	}
	if err := scanner.Err(); err != nil {
		return Config{}, nil, err
	}
	return c, notes, nil
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
	case "system.usb_autosuspend":
		c.System.USBAutosuspend, err = intValue()
	case "grbl.backend":
		c.GRBL.Backend, err = stringValue()
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
	case "grbl.on_disconnect":
		c.GRBL.OnDisconnect, err = stringValue()
	case "grbl.monitor_port":
		c.GRBL.MonitorPort, err = intValue()
	case "grbl.stationary_beam_seconds":
		c.GRBL.StationaryBeamSeconds, err = intValue()
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
		return fmt.Errorf("%w %s.%s", errUnknownKey, section, key)
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
	fmt.Fprintf(&b, "system:\n  hostname: %s\n  setup_complete: %t\n  usb_autosuspend: %d\n", q(c.System.Hostname), c.System.SetupComplete, c.System.USBAutosuspend)
	fmt.Fprintf(&b, "grbl:\n  backend: %s\n  device: %s\n  baudrate: %d\n  port: %d\n  max_connections: %d\n  reconnect: %t\n  kick_old_user: %t\n  on_disconnect: %s\n  monitor_port: %d\n  stationary_beam_seconds: %d\n",
		q(c.GRBL.Backend), q(c.GRBL.Device), c.GRBL.Baudrate, c.GRBL.Port, c.GRBL.MaxConnections, c.GRBL.Reconnect, c.GRBL.KickOldUser, q(c.GRBL.OnDisconnect), c.GRBL.MonitorPort, c.GRBL.StationaryBeamSeconds)
	fmt.Fprintf(&b, "camera:\n  device: %s\n  format: %s\n  resolution: %s\n  fps: %d\n  port: %d\n  quality: %d\n",
		q(c.Camera.Device), q(c.Camera.Format), q(c.Camera.Resolution), c.Camera.FPS, c.Camera.Port, c.Camera.Quality)
	fmt.Fprintf(&b, "ssh:\n  enabled: %t\n  password_authentication: %t\n", c.SSH.Enabled, c.SSH.PasswordAuthentication)
	fmt.Fprintf(&b, "network:\n  mode: %s\n  address: %s\n  gateway: %s\n  dns: %s\n",
		q(c.Network.Mode), q(c.Network.Address), q(c.Network.Gateway), q(strings.Join(c.Network.DNS, ",")))
	fmt.Fprintf(&b, "wifi:\n  enabled: %t\n  ssid: %s\n  psk: %s\n  country: %s\n  hidden: %t\n",
		c.WiFi.Enabled, q(c.WiFi.SSID), q(c.WiFi.PSK), q(c.WiFi.Country), c.WiFi.Hidden)
	return []byte(b.String()), nil
}
