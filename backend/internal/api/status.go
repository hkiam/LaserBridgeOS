package api

import (
	"bufio"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
)

type statusResponse struct {
	Hostname    string             `json:"hostname"`
	IPAddresses []string           `json:"ip_addresses"`
	Uptime      uint64             `json:"uptime_seconds"`
	CPUPercent  float64            `json:"cpu_percent"`
	Memory      memoryStatus       `json:"memory"`
	Storage     storageStatus      `json:"storage"`
	Services    map[string]bool    `json:"services"`
	Devices     map[string]bool    `json:"devices"`
	Clients     map[string]int     `json:"clients"`
	Version     string             `json:"version"`
	Config      statusConfigFields `json:"config"`
	// ConfigWarnings is what the stored configuration contained that this
	// version did not understand, and what was done about it. Usually empty;
	// after a rollback to an older slot it is the difference between "the
	// appliance forgot my settings" and "this version is older than the file".
	ConfigWarnings []string `json:"config_warnings,omitempty"`
}

type memoryStatus struct {
	UsedBytes  uint64 `json:"used_bytes"`
	TotalBytes uint64 `json:"total_bytes"`
}

type storageStatus struct {
	FreeBytes  uint64 `json:"free_bytes"`
	TotalBytes uint64 `json:"total_bytes"`
}

type statusConfigFields struct {
	// GRBLBackend is which of the two services owns the serial port, so the
	// web interface reports on the one that is actually running.
	GRBLBackend     string `json:"grbl_backend"`
	GRBLDevice      string `json:"grbl_device"`
	GRBLPort        int    `json:"grbl_port"`
	CameraDevice    string `json:"camera_device"`
	CameraStreamURL string `json:"camera_stream_url"`
	CameraMode      string `json:"camera_mode"`
	// USBAutosuspend is what the configuration asks of the kernel;
	// USBAutosuspendActive is what the kernel answers when asked back, and is
	// null when it could not be read. The two are shown side by side because
	// they can differ - a backend without root cannot write sysfs - and a page
	// that only showed the saved value would be reporting an intention as a
	// fact.
	USBAutosuspend       int  `json:"usb_autosuspend"`
	USBAutosuspendActive *int `json:"usb_autosuspend_active"`
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	cfg, err := s.Store.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = cfg.System.Hostname
	}
	sys := s.system()
	services := map[string]bool{}
	// The watchdog is listed with the rest because it is the one service whose
	// absence is invisible by design: a board without /dev/watchdog refuses to
	// start it, and an appliance with no last resort must not look like one
	// that has it (ADR 0017).
	for _, name := range []string{"ser2net", "laserbridged", "ustreamer", "sshd", "avahi-daemon", "laserbridge-web", "laserbridge-watchdog"} {
		services[name] = s.serviceRunning(name)
	}
	// supervise-daemon reports "started" for as long as the supervisor lives,
	// even while the program it supervises exits on every attempt. That is
	// how a GRBL bridge that never once managed to bind its port could be
	// reported as running. Where a service exists to answer on a port, ask
	// the port.
	// Whichever backend owns the GRBL port has to be listening on it to
	// count as running, and the one that is not selected is simply off.
	services["ser2net"] = services["ser2net"] && sys.listening(cfg.GRBL.Port)
	// And whichever one is laserbridged has to still be answering. A wedged
	// daemon keeps its listening socket - the kernel accepts on its behalf -
	// so the port alone proves nothing, and reporting a bridge that has
	// stopped supervising the machine as "running" is the wrong answer.
	services["laserbridged"] = services["laserbridged"] && sys.listening(cfg.GRBL.Port) &&
		// Only asked when it is the backend in charge: with ser2net selected
		// there is nothing at the other end of that socket, and dialling it on
		// every status poll is work for no answer.
		(cfg.GRBL.Backend != config.BackendLaserbridged ||
			s.BridgeWatch == nil || s.BridgeWatch.Responsive())
	services["ustreamer"] = services["ustreamer"] && sys.listening(cfg.Camera.Port)
	version := readTrimmed(s.VersionPath)
	if version == "" {
		version = "development"
	}
	var usbActive *int
	if s.Runtime != nil {
		if value, ok := s.Runtime.USBAutosuspend(); ok {
			usbActive = &value
		}
	}
	writeJSON(w, http.StatusOK, statusResponse{
		Hostname: hostname, IPAddresses: uniqueAddresses(), Uptime: sys.uptime(), CPUPercent: sys.cpuPercent(),
		Memory: sys.memory(), Storage: storage(s.dataPath()), Services: services,
		Devices: map[string]bool{"grbl": sys.exists(cfg.GRBL.Device), "camera": sys.exists(cfg.Camera.Device)},
		Clients: map[string]int{"grbl": sys.tcpClients(cfg.GRBL.Port)}, Version: version,
		ConfigWarnings: s.Store.Notes(),
		Config: statusConfigFields{
			GRBLBackend: cfg.GRBL.Backend,
			GRBLDevice:  cfg.GRBL.Device, GRBLPort: cfg.GRBL.Port, CameraDevice: cfg.Camera.Device,
			CameraStreamURL: "http://" + cfg.System.Hostname + ".local:" + strconv.Itoa(cfg.Camera.Port) + "/stream",
			CameraMode:      cfg.Camera.Resolution + " @ " + strconv.Itoa(cfg.Camera.FPS) + " FPS",
			USBAutosuspend:  cfg.System.USBAutosuspend, USBAutosuspendActive: usbActive,
		},
	})
}

func (s *Server) serviceRunning(name string) bool {
	if s.Runner == nil {
		return false
	}
	_, err := s.Runner.Run("rc-service", name, "status")
	return err == nil
}

func (r system) uptime() uint64 {
	fields := strings.Fields(readTrimmed(r.path("/proc/uptime")))
	if len(fields) == 0 {
		return 0
	}
	seconds, _ := strconv.ParseFloat(fields[0], 64)
	return uint64(seconds)
}

func (r system) cpuSnapshot() (idle, total uint64) {
	file, err := os.Open(r.path("/proc/stat"))
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return 0, 0
	}
	fields := strings.Fields(scanner.Text())
	for i, raw := range fields[1:] {
		value, _ := strconv.ParseUint(raw, 10, 64)
		total += value
		if i == 3 || i == 4 {
			idle += value
		}
	}
	return idle, total
}

func (r system) cpuPercent() float64 {
	idleA, totalA := r.cpuSnapshot()
	time.Sleep(50 * time.Millisecond)
	idleB, totalB := r.cpuSnapshot()
	if totalB <= totalA {
		return 0
	}
	value := 100 * (1 - float64(idleB-idleA)/float64(totalB-totalA))
	return float64(int(value*10+0.5)) / 10
}

func (r system) memory() memoryStatus {
	values := map[string]uint64{}
	file, err := os.Open(r.path("/proc/meminfo"))
	if err != nil {
		return memoryStatus{}
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			values[strings.TrimSuffix(fields[0], ":")], _ = strconv.ParseUint(fields[1], 10, 64)
		}
	}
	total := values["MemTotal"] * 1024
	available := values["MemAvailable"] * 1024
	return memoryStatus{UsedBytes: total - available, TotalBytes: total}
}

func storage(path string) storageStatus {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return storageStatus{}
	}
	return storageStatus{FreeBytes: stat.Bavail * uint64(stat.Bsize), TotalBytes: stat.Blocks * uint64(stat.Bsize)}
}

// listening reports whether anything holds a listening socket on the port.
func (r system) listening(port int) bool {
	return r.countTCP(port, "0A") > 0
}

func (r system) tcpClients(port int) int {
	return r.countTCP(port, "01")
}

// countTCP counts sockets on a local port in the given state, as /proc spells
// it: 0A is LISTEN, 01 is ESTABLISHED.
func (r system) countTCP(port int, state string) int {
	needle := strings.ToUpper(strconv.FormatInt(int64(port), 16))
	needle = strings.Repeat("0", 4-len(needle)) + needle
	count := 0
	for _, path := range []string{r.path("/proc/net/tcp"), r.path("/proc/net/tcp6")} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) > 3 && strings.HasSuffix(fields[1], ":"+needle) && fields[3] == state {
				count++
			}
		}
	}
	return count
}
