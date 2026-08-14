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

func TestParserRejectsUnknownKey(t *testing.T) {
	data, _ := MarshalYAML(Default())
	data = append(data, []byte("  unexpected: true\n")...)
	if _, err := ParseYAML(data); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("ParseYAML() = %v, want unknown key", err)
	}
}

func TestStoreWritesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	store := NewStore(path)
	if err := store.Ensure(); err != nil {
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
