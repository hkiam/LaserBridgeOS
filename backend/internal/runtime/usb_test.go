package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
)

// fakeSysfs builds the parts of /sys this policy touches: the module parameter
// that governs devices probed from now on, a root hub and a device that are
// already there, and an interface that has a power directory but none of the
// attributes - which is what a real /sys/bus/usb/devices looks like, and the
// shape that made an earlier version create files where the kernel has none.
func fakeSysfs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, value string) {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, usbAutosuspendParameter), "2")
	for _, device := range []string{"usb1", "1-1"} {
		write(filepath.Join(root, usbDeviceDirectory, device, "power", "control"), "auto")
		write(filepath.Join(root, usbDeviceDirectory, device, "power", "autosuspend_delay_ms"), "2000")
	}
	if err := os.MkdirAll(filepath.Join(root, usbDeviceDirectory, "1-1:1.0", "power"), 0755); err != nil {
		t.Fatal(err)
	}
	return root
}

func read(t *testing.T, parts ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func TestUSBAutosuspendIsOffByDefault(t *testing.T) {
	root := fakeSysfs(t)
	manager := &Manager{Root: root}
	cfg := config.Default()
	if cfg.System.USBAutosuspend != config.USBAutosuspendOff {
		t.Fatalf("default usb_autosuspend = %d, want %d", cfg.System.USBAutosuspend, config.USBAutosuspendOff)
	}
	if err := manager.ApplyUSBPolicy(cfg); err != nil {
		t.Fatal(err)
	}
	if value := read(t, root, usbAutosuspendParameter); value != "-1" {
		t.Errorf("usbcore autosuspend parameter = %q, want -1", value)
	}
	// The parameter alone would leave the adapter that has been plugged in
	// since boot exactly as it was, which is the device that matters.
	for _, device := range []string{"usb1", "1-1"} {
		if value := read(t, root, usbDeviceDirectory, device, "power", "control"); value != "on" {
			t.Errorf("%s power/control = %q, want on", device, value)
		}
	}
	if _, err := os.Stat(filepath.Join(root, usbDeviceDirectory, "1-1:1.0", "power", "control")); !os.IsNotExist(err) {
		t.Errorf("an attribute the kernel does not have was created: %v", err)
	}
}

func TestUSBAutosuspendCanBeTurnedBackOn(t *testing.T) {
	root := fakeSysfs(t)
	manager := &Manager{Root: root}
	cfg := config.Default()
	cfg.System.USBAutosuspend = 5
	if err := manager.ApplyUSBPolicy(cfg); err != nil {
		t.Fatal(err)
	}
	if value := read(t, root, usbAutosuspendParameter); value != "5" {
		t.Errorf("usbcore autosuspend parameter = %q, want 5", value)
	}
	for _, device := range []string{"usb1", "1-1"} {
		if value := read(t, root, usbDeviceDirectory, device, "power", "autosuspend_delay_ms"); value != "5000" {
			t.Errorf("%s autosuspend_delay_ms = %q, want 5000", device, value)
		}
		if value := read(t, root, usbDeviceDirectory, device, "power", "control"); value != "auto" {
			t.Errorf("%s power/control = %q, want auto", device, value)
		}
	}
	active, ok := manager.USBAutosuspend()
	if !ok || active != 5 {
		t.Errorf("USBAutosuspend() = %d, %t; want 5, true", active, ok)
	}
}

func TestApplySurvivesAKernelWithoutUSBPowerManagement(t *testing.T) {
	// A development machine, a container, a kernel built without CONFIG_PM: the
	// knob is absent rather than broken, and the rest of Apply - hostname,
	// sshd, the bridge configuration - must still land.
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "data", "config.yaml"))
	if _, err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Store: store, Dir: filepath.Join(dir, "run"), Run: &recordRunner{}, Root: filepath.Join(dir, "no-sys")}
	if err := manager.Apply(); err != nil {
		t.Fatalf("Apply() = %v, want a missing usbcore to be an absence, not a failure", err)
	}
	if _, ok := manager.USBAutosuspend(); ok {
		t.Error("USBAutosuspend() claimed to know a value that cannot be read")
	}
}
