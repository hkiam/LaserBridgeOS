package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
)

type recordRunner struct {
	calls []string
	out   []byte
}

func (r *recordRunner) Run(name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	return r.out, nil
}

func TestApplyGeneratesRuntimeConfiguration(t *testing.T) {
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "data", "config.yaml"))
	if _, err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	runner := &recordRunner{}
	manager := &Manager{Store: store, Dir: filepath.Join(dir, "run"), Run: runner}
	if err := manager.Apply(); err != nil {
		t.Fatal(err)
	}
	ser2net, err := os.ReadFile(filepath.Join(manager.Dir, "ser2net.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"tcp,23", "/dev/ttyUSB0,115200n81", "max-connections: 1", "kickolduser: true"} {
		if !strings.Contains(string(ser2net), expected) {
			t.Errorf("ser2net config lacks %q:\n%s", expected, ser2net)
		}
	}
	sshd, _ := os.ReadFile(filepath.Join(manager.Dir, "sshd_config"))
	// Password login is a deliberate default (ADR 0006); root login and empty
	// passwords are not, and must stay off whatever else changes.
	for _, forbidden := range []string{"PermitRootLogin yes", "PermitEmptyPasswords yes"} {
		if strings.Contains(string(sshd), forbidden) {
			t.Fatalf("unsafe ssh setting %q:\n%s", forbidden, sshd)
		}
	}
	for _, required := range []string{"PasswordAuthentication yes", "AllowUsers laserbridge", "PermitRootLogin no"} {
		if !strings.Contains(string(sshd), required) {
			t.Fatalf("sshd config lacks %q:\n%s", required, sshd)
		}
	}
	if len(runner.calls) != 1 || runner.calls[0] != "hostname laserbridge" {
		t.Fatalf("calls = %v", runner.calls)
	}
	mode, _ := os.ReadFile(filepath.Join(manager.Dir, "network-mode"))
	if string(mode) != "ap\n" {
		t.Fatalf("first-boot network mode = %q", mode)
	}
	hostapd, _ := os.ReadFile(filepath.Join(manager.Dir, "hostapd.conf"))
	if !strings.Contains(string(hostapd), "ssid=LaserBridge-Setup") || !strings.Contains(string(hostapd), "wpa=2") {
		t.Fatalf("invalid hostapd config:\n%s", hostapd)
	}

	cfg, _ := store.Load()
	cfg.System.SetupComplete = true
	cfg.WiFi = config.WiFi{Enabled: true, SSID: `Shop "WiFi"`, PSK: `secret\\pass`, Country: "DE", Hidden: true}
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := manager.Apply(); err != nil {
		t.Fatal(err)
	}
	mode, _ = os.ReadFile(filepath.Join(manager.Dir, "network-mode"))
	if string(mode) != "client\n" {
		t.Fatalf("configured network mode = %q", mode)
	}
	wpa, _ := os.ReadFile(filepath.Join(manager.Dir, "wpa_supplicant.conf"))
	if !strings.Contains(string(wpa), `ssid="Shop \"WiFi\""`) || !strings.Contains(string(wpa), `psk="secret\\\\pass"`) || !strings.Contains(string(wpa), "scan_ssid=1") {
		t.Fatalf("invalid wpa_supplicant config:\n%s", wpa)
	}
}

func TestUstreamerArgumentsAreSeparate(t *testing.T) {
	cfg := config.Default()
	manager := &Manager{}
	args := manager.UstreamerArgs(cfg, "MJPEG")
	want := []string{"--device=/dev/video0", "--format=MJPEG", "--resolution=1280x720", "--desired-fps=30", "--host=0.0.0.0", "--port=8080"}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}
