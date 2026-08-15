package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
	lbruntime "github.com/laserbridgeos/laserbridgeos/backend/internal/runtime"
)

// The status page reports on the machine it runs on, which used to mean it read
// the developer's machine during the tests - /proc for the load, /sys for the
// wireless interface, /dev for the devices - and no assertion about any of it
// was possible. With one root for all of them the whole page can be pointed at
// a directory built here, and what it says is checkable rather than whatever
// the host happened to be doing.
func TestStatusReadsTheSystemItWasPointedAt(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("/proc/uptime", "8130.42 32000.00\n")
	write("/proc/stat", "cpu  100 0 100 800 0 0 0 0 0 0\n")
	write("/proc/meminfo", "MemTotal:       2048000 kB\nMemAvailable:   1024000 kB\n")
	write("/proc/net/tcp", "  sl  local_address rem_address   st\n")
	write("/proc/net/tcp6", "  sl  local_address rem_address   st\n")
	write("/dev/ttyUSB0", "")
	write("/sys/module/usbcore/parameters/autosuspend", "-1\n")

	store := config.NewStore(filepath.Join(dir, "config.yaml"))
	if _, err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	manager := &lbruntime.Manager{Store: store, Dir: filepath.Join(dir, "run"), DataDir: filepath.Join(dir, "data"), Root: root}
	handler := (&Server{Store: store, Runtime: manager, Root: root}).Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var body statusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Uptime != 8130 {
		t.Errorf("uptime = %d, want the 8130 seconds in the fake /proc", body.Uptime)
	}
	if body.Memory.TotalBytes != 2048000*1024 || body.Memory.UsedBytes != 1024000*1024 {
		t.Errorf("memory = %+v, want it read from the fake /proc/meminfo", body.Memory)
	}
	// The device is named the way the appliance knows it, not the way it was
	// found: /dev/ttyUSB0 is what goes into the configuration and what the
	// bridge opens.
	if !body.Devices["grbl"] {
		t.Error("the GRBL device under the root was not found")
	}
	if body.Devices["camera"] {
		t.Error("a camera was reported that does not exist under the root")
	}
	if body.Config.USBAutosuspendActive == nil || *body.Config.USBAutosuspendActive != -1 {
		t.Errorf("usb_autosuspend_active = %v, want the -1 in the fake sysfs", body.Config.USBAutosuspendActive)
	}
}
