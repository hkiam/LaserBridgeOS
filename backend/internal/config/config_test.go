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
	parsed, err := ParseYAML(written)
	if err != nil {
		t.Fatal(err)
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
		{"port collision", func(c *Config) { c.Camera.Port = 23 }, "unique"},
		{"oversized camera", func(c *Config) { c.Camera.Resolution = "9000x720" }, "too large"},
		{"unknown disconnect action", func(c *Config) { c.GRBL.OnDisconnect = "stop" }, "on_disconnect"},
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
	// no such key. It must load, and it must land on the cautious setting
	// rather than on the zero value, which is not a valid action at all.
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
	parsed, err := ParseYAML([]byte(strings.Join(kept, "\n")))
	if err != nil {
		t.Fatalf("ParseYAML() = %v", err)
	}
	if parsed.GRBL.OnDisconnect != DisconnectHold {
		t.Errorf("on_disconnect = %q, want %q", parsed.GRBL.OnDisconnect, DisconnectHold)
	}
}

func TestParserRejectsUnknownKey(t *testing.T) {
	data, _ := MarshalYAML(Default())
	data = append(data, []byte("  unexpected: true\n")...)
	if _, err := ParseYAML(data); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("ParseYAML() = %v, want unknown key", err)
	}
}

func TestEnsureQuarantinesUnusableConfig(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"unparsable", "system:\n  hostname: \"laserbridge\"\n  surprise: yes\n"},
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
