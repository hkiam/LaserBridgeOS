package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type scanRunner struct {
	name string
	args []string
	out  []byte
	err  error
}

func (r *scanRunner) Run(name string, args ...string) ([]byte, error) {
	r.name = name
	r.args = append([]string(nil), args...)
	return r.out, r.err
}

const sampleIWScan = `BSS 00:11:22:33:44:55(on wlan0)
	freq: 2412
	signal: -62.50 dBm
	capability: ESS Privacy (0x0411)
	SSID: Workshop
	RSN:
		Authentication suites: PSK
BSS 00:11:22:33:44:66(on wlan0)
	freq: 5180
	signal: -38.00 dBm
	capability: ESS Privacy (0x0411)
	SSID: Workshop
	RSN:
		Authentication suites: SAE
BSS aa:bb:cc:dd:ee:ff(on wlan0)
	freq: 2437
	signal: -51.00 dBm
	capability: ESS (0x0401)
	SSID: Guest\x20WiFi
`

func TestParseIWScanDeduplicatesAndSorts(t *testing.T) {
	networks := parseIWScan([]byte(sampleIWScan))
	if len(networks) != 2 {
		t.Fatalf("networks = %#v", networks)
	}
	if networks[0].SSID != "Workshop" || networks[0].Signal != -38 || networks[0].Security != "WPA3" {
		t.Fatalf("strongest network = %#v", networks[0])
	}
	if networks[1].SSID != "Guest WiFi" || networks[1].Security != "Open" {
		t.Fatalf("decoded open network = %#v", networks[1])
	}
}

func TestWiFiScanUsesDiscoveredInterfaceWithoutShell(t *testing.T) {
	sysfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sysfs, "wlan-test", "wireless"), 0755); err != nil {
		t.Fatal(err)
	}
	runner := &scanRunner{out: []byte(sampleIWScan)}
	handler := (&Server{Runner: runner, WirelessSysfs: sysfs}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/wifi/scan", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if runner.name != "iw" || !reflect.DeepEqual(runner.args, []string{"dev", "wlan-test", "scan"}) {
		t.Fatalf("runner call = %q %#v", runner.name, runner.args)
	}
}
