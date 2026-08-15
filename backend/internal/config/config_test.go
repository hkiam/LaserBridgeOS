package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultRoundTrip(t *testing.T) {
	written, err := MarshalYAML(Default())
	if err != nil {
		t.Fatal(err)
	}
	parsed, notes, err := ParseYAML(written)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 0 {
		t.Fatalf("our own output was not fully understood: %v", notes)
	}
	if !reflect.DeepEqual(parsed, Default()) {
		t.Fatalf("round trip changed config:\n%s", written)
	}
}

func TestRejectsUnsafeValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"device traversal", func(c *Config) { c.GRBL.Device = "/dev/ttyUSB0/../x" }, "device path"},
		{"hostname injection", func(c *Config) { c.System.Hostname = "laser;reboot" }, "hostname"},
		{"port collision", func(c *Config) { c.Camera.Port = 23 }, "belongs to grbl.port"},
		{"monitor on the web port", func(c *Config) { c.GRBL.MonitorPort = 80 }, "the web interface"},
		{"monitor on the bridge port", func(c *Config) { c.GRBL.MonitorPort = 23 }, "belongs to grbl.port"},
		{"oversized camera", func(c *Config) { c.Camera.Resolution = "9000x720" }, "too large"},
		{"unknown disconnect action", func(c *Config) { c.GRBL.OnDisconnect = "stop" }, "on_disconnect"},
		{"autosuspend below the off sentinel", func(c *Config) { c.System.USBAutosuspend = -2 }, "usb_autosuspend"},
		{"autosuspend beyond an hour", func(c *Config) { c.System.USBAutosuspend = 3601 }, "usb_autosuspend"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Default()
			test.mutate(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestOlderConfigGetsSafeDisconnectDefault(t *testing.T) {
	// A configuration written before the bridge could act on a disconnect has
	// no such key. It must load, and it must land on the setting that switches
	// the beam off whatever the controller is configured like - not on the
	// zero value, which is not a valid action at all.
	data, err := MarshalYAML(Default())
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "on_disconnect") {
			kept = append(kept, line)
		}
	}
	parsed, _, err := ParseYAML([]byte(strings.Join(kept, "\n")))
	if err != nil {
		t.Fatalf("ParseYAML() = %v", err)
	}
	if parsed.GRBL.OnDisconnect != DisconnectReset {
		t.Errorf("on_disconnect = %q, want %q", parsed.GRBL.OnDisconnect, DisconnectHold)
	}
}

func TestOlderConfigGetsAutosuspendOff(t *testing.T) {
	// An appliance that was configured before this key existed carries none.
	// Loading it must land on "off" rather than on the zero value, which is the
	// kernel's instruction to suspend an idle adapter immediately - the exact
	// behaviour this default exists to prevent, silently applied to the
	// installations that never asked for it.
	data, err := MarshalYAML(Default())
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "usb_autosuspend") {
			kept = append(kept, line)
		}
	}
	parsed, _, err := ParseYAML([]byte(strings.Join(kept, "\n")))
	if err != nil {
		t.Fatalf("ParseYAML() = %v", err)
	}
	if parsed.System.USBAutosuspend != USBAutosuspendOff {
		t.Errorf("usb_autosuspend = %d, want %d", parsed.System.USBAutosuspend, USBAutosuspendOff)
	}
}

func TestConfigurationFromANewerVersionSurvivesARollback(t *testing.T) {
	// The scenario this tolerance exists for. /data is shared by both system
	// slots, so an appliance that falls back to the previous slot after a bad
	// update hands an older binary a file a newer one wrote. Refusing it would
	// quarantine the configuration and take the appliance off the network -
	// during the recovery that was supposed to save it.
	data, err := MarshalYAML(Default())
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := ParseYAML(data)
	if err != nil {
		t.Fatal(err)
	}
	stored.WiFi = WiFi{Enabled: true, SSID: "Workshop", PSK: "cutscutscuts", Country: "DE"}
	stored.System.SetupComplete = true
	data, err = MarshalYAML(stored)
	if err != nil {
		t.Fatal(err)
	}
	// What a later release might add: a key in a section this version knows,
	// and a whole section it does not.
	newer := string(data) + "  spindle_warmup_seconds: 8\ncoolant:\n  enabled: true\n  delay: 4\n"

	parsed, notes, err := ParseYAML([]byte(newer))
	if err != nil {
		t.Fatalf("ParseYAML() = %v, want the file to be readable by an older version", err)
	}
	if !reflect.DeepEqual(parsed, stored) {
		t.Errorf("parsed = %+v, want everything this version does understand", parsed)
	}
	if len(notes) != 2 {
		t.Errorf("notes = %v, want one for the unknown key and one for the unknown section", notes)
	}
}

func TestARolledBackApplianceLeavesTheNewerKeysAlone(t *testing.T) {
	// Reading the file must not rewrite it. The keys this version dropped on
	// the way in are the newer version's settings, and booting its slot again
	// has to find them where it left them.
	path := filepath.Join(t.TempDir(), "config.yaml")
	data, _ := MarshalYAML(Default())
	stored := string(data) + "  spindle_warmup_seconds: 8\n"
	if err := os.WriteFile(path, []byte(stored), 0640); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)
	quarantined, err := store.Ensure()
	if err != nil || quarantined != "" {
		t.Fatalf("Ensure() = %q, %v; want the file accepted as it is", quarantined, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != stored {
		t.Errorf("the file was rewritten and the newer setting is gone:\n%s", after)
	}
	if notes := store.Notes(); len(notes) != 1 || !strings.Contains(notes[0], "spindle_warmup_seconds") {
		t.Errorf("Notes() = %v, want the ignored key named", notes)
	}
}

func TestAnUnusableValueCostsOnlyItsOwnField(t *testing.T) {
	// A range or spelling a later release widened reaches an older one as a
	// value it cannot read. That field falls back to its default; the other
	// twenty are none of its business.
	data, _ := MarshalYAML(Default())
	broken := strings.Replace(string(data), "baudrate: 115200", "baudrate: quick", 1)
	parsed, notes, err := ParseYAML([]byte(broken))
	if err != nil {
		t.Fatalf("ParseYAML() = %v", err)
	}
	if parsed.GRBL.Baudrate != Default().GRBL.Baudrate {
		t.Errorf("baudrate = %d, want the default", parsed.GRBL.Baudrate)
	}
	if parsed.GRBL.Port != Default().GRBL.Port || parsed.Camera.FPS != Default().Camera.FPS {
		t.Error("one unreadable value took other settings with it")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "baudrate") {
		t.Errorf("notes = %v, want the field named", notes)
	}
}

func TestEnsureQuarantinesUnusableConfig(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		// Unknown names are tolerated; an unknown shape is not, because at that
		// point "written by a newer version" and "damaged" look the same.
		{"malformed shape", "system:\n    hostname: \"laserbridge\"\n"},
		{"invalid", "system:\n  hostname: \"not a hostname\"\n"},
		{"truncated", "system:\n  hostn"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(test.content), 0640); err != nil {
				t.Fatal(err)
			}
			store := NewStore(path)
			quarantined, err := store.Ensure()
			if err != nil {
				t.Fatalf("Ensure() = %v, want the appliance to recover", err)
			}
			if quarantined != path+".broken" {
				t.Fatalf("quarantined = %q, want %q", quarantined, path+".broken")
			}
			kept, err := os.ReadFile(quarantined)
			if err != nil || string(kept) != test.content {
				t.Fatalf("broken config not preserved: %q, %v", kept, err)
			}
			loaded, err := store.Load()
			if err != nil {
				t.Fatalf("Load() after recovery = %v", err)
			}
			if !reflect.DeepEqual(loaded, Default()) {
				t.Fatalf("recovered config = %+v, want defaults", loaded)
			}
		})
	}
}

func TestQuarantineKeepsTheApplianceOnItsNetwork(t *testing.T) {
	// The failure this guards: a configuration that cannot be used was replaced
	// by the defaults outright, which forgets the Wi-Fi network and puts the
	// appliance back into first-boot access-point mode. A headless box that
	// leaves the network it was reachable on is a much worse outcome than one
	// whose camera resolution went back to 1280x720.
	path := filepath.Join(t.TempDir(), "config.yaml")
	stored := Default()
	stored.System.Hostname = "cutter"
	stored.System.SetupComplete = true
	stored.WiFi = WiFi{Enabled: true, SSID: "Workshop", PSK: "cutscutscuts", Country: "DE"}
	stored.Camera.FPS = 15
	data, err := MarshalYAML(stored)
	if err != nil {
		t.Fatal(err)
	}
	// A value only a later release considers valid - here a port number this
	// version rejects outright.
	broken := strings.Replace(string(data), "port: 23", "port: 70000", 1)
	if err := os.WriteFile(path, []byte(broken), 0640); err != nil {
		t.Fatal(err)
	}

	store := NewStore(path)
	quarantined, err := store.Ensure()
	if err != nil {
		t.Fatalf("Ensure() = %v", err)
	}
	if quarantined != path+".broken" {
		t.Fatalf("quarantined = %q, want the file kept", quarantined)
	}
	recovered, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.System.SetupComplete {
		t.Error("setup_complete was cleared; the appliance would start its setup access point")
	}
	if recovered.WiFi != stored.WiFi {
		t.Errorf("wifi = %+v, want the credentials kept: %+v", recovered.WiFi, stored.WiFi)
	}
	if recovered.System.Hostname != "cutter" {
		t.Errorf("hostname = %q, want it kept - it is how the appliance is found", recovered.System.Hostname)
	}
	// And the rest is back to defaults, which is what the web interface is for.
	if recovered.GRBL.Port != Default().GRBL.Port || recovered.Camera.FPS != Default().Camera.FPS {
		t.Errorf("recovered = %+v, want defaults outside the network settings", recovered)
	}
	if notes := store.Notes(); len(notes) != 1 || !strings.Contains(notes[0], ".broken") {
		t.Errorf("Notes() = %v, want the quarantine reported", notes)
	}
}

func TestQuarantineFallsBackToTheAccessPointWithoutUsableWiFi(t *testing.T) {
	// Keeping setup_complete is only right while there is still a way onto the
	// network. A Wi-Fi appliance whose credentials could not be salvaged has
	// none, and the setup access point is exactly the fallback for that.
	path := filepath.Join(t.TempDir(), "config.yaml")
	stored := Default()
	stored.System.SetupComplete = true
	stored.WiFi = WiFi{Enabled: true, SSID: "Workshop", PSK: "cutscutscuts", Country: "DE"}
	data, _ := MarshalYAML(stored)
	broken := strings.Replace(string(data), `psk: "cutscutscuts"`, `psk: "short"`, 1)
	if err := os.WriteFile(path, []byte(broken), 0640); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)
	if _, err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.System.SetupComplete {
		t.Error("the appliance kept setup_complete with no way back onto the network")
	}
}

func TestEnsureKeepsUsableConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	store := NewStore(path)
	if _, err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg, _ := store.Load()
	cfg.GRBL.Port = 2323
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	quarantined, err := store.Ensure()
	if err != nil || quarantined != "" {
		t.Fatalf("Ensure() = %q, %v; want no recovery", quarantined, err)
	}
	loaded, _ := store.Load()
	if loaded.GRBL.Port != 2323 {
		t.Fatalf("port = %d, want the stored 2323", loaded.GRBL.Port)
	}
}

func TestStoreWritesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	store := NewStore(path)
	if _, err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.GRBL.Port = 2323
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}
	loaded, _ := store.Load()
	if loaded.GRBL.Port != 2323 {
		t.Fatalf("port = %d, want 2323", loaded.GRBL.Port)
	}
}
