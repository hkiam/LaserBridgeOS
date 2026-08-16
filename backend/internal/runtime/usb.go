package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
)

// USB autosuspend is switched off by default, and the reason is the machine at
// the other end of the cable. A CH340 or FTDI adapter that the kernel suspends
// between two G-code lines wakes up on the next write, and the first characters
// of that write are the ones that pay for it: a dropped byte in a stream GRBL
// parses line by line is a wrong cut, not a failed job. Some adapters do worse
// and re-enumerate, which moves /dev/ttyUSB0 out from under the bridge in the
// middle of a job.
//
// Two knobs decide this, and telling only one of them is the mistake worth
// documenting:
//
//   - /sys/module/usbcore/parameters/autosuspend is the default delay handed to
//     devices as they are probed. Writing it changes nothing about the adapter
//     that has been plugged in since boot.
//   - /sys/bus/usb/devices/*/power/ carries what each already-probed device is
//     actually doing, and that is where the controller and the camera live by
//     the time anyone opens the web interface.
//
// So the parameter is set for whatever gets plugged in next, and every device
// that is already there is walked and told the same thing.
const (
	usbAutosuspendParameter = "/sys/module/usbcore/parameters/autosuspend"
	usbDeviceDirectory      = "/sys/bus/usb/devices"
)

// ApplyUSBPolicy puts config.System.USBAutosuspend into effect.
//
// It reports the first thing that would not take, having tried the rest: a
// single device whose attributes have gone away - unplugged while the sweep
// ran - must not stop the sweep from reaching the others.
func (m *Manager) ApplyUSBPolicy(cfg config.Config) error {
	seconds := cfg.System.USBAutosuspend
	parameter := m.Path(usbAutosuspendParameter)
	if err := writeSysfs(parameter, strconv.Itoa(seconds)); err != nil {
		return fmt.Errorf("set the USB autosuspend default: %w", err)
	}

	entries, err := os.ReadDir(m.Path(usbDeviceDirectory))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("list USB devices: %w", err)
	}
	var failure error
	for _, entry := range entries {
		power := m.Path(usbDeviceDirectory, entry.Name(), "power")
		// The delay goes first. Setting control to auto while the old delay is
		// still in place would open a window - short, but during a job - in
		// which the device suspends on a schedule nobody asked for.
		//
		// And it is set in both directions. A negative delay prevents
		// autosuspend on its own, whatever power/control says afterwards - and
		// something else may well say something afterwards: a driver, mdev, a
		// device that re-enumerates. Setting only control leaves the decision
		// resting on a value anybody can change, and a device that appears
		// later gets a negative delay from the module parameter anyway, so this
		// merely makes the ones we touched agree with the ones we did not.
		delay := strconv.Itoa(seconds * 1000)
		if seconds < 0 {
			delay = "-1000"
		}
		if err := writeSysfs(filepath.Join(power, "autosuspend_delay_ms"), delay); err != nil && failure == nil {
			failure = fmt.Errorf("set the autosuspend delay of %s: %w", entry.Name(), err)
		}
		if err := writeSysfs(filepath.Join(power, "control"), usbControl(seconds)); err != nil && failure == nil {
			failure = fmt.Errorf("set the power control of %s: %w", entry.Name(), err)
		}
	}
	return failure
}

// usbControl spells the policy the way the power/control attribute does. "on"
// means the device stays awake, "auto" means the kernel may suspend it after
// the delay.
func usbControl(seconds int) string {
	if seconds < 0 {
		return "on"
	}
	return "auto"
}

// USBAutosuspend reports the default delay usbcore is currently using, and
// whether it could be read at all. The web interface shows this beside the
// configured value: the two disagreeing is the visible symptom of a kernel that
// was never told - the backend running without the privileges to write sysfs,
// most likely - and inventing a number for it would hide exactly that.
func (m *Manager) USBAutosuspend() (int, bool) {
	data, err := os.ReadFile(m.Path(usbAutosuspendParameter))
	if err != nil {
		return 0, false
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	return value, true
}

// writeSysfs sets a kernel attribute, and only a kernel attribute.
//
// The file is opened without O_CREATE on purpose. A sysfs attribute that is not
// there is a kernel that does not have it - an interface with no power/control,
// a build without CONFIG_PM - and creating a regular file in its place would
// turn "this knob does not exist" into a silent success, in the tests most of
// all. O_TRUNC is there for the opposite reason: sysfs takes the write whole
// and ignores the length, but a plain file under a test's fake /sys would keep
// the tail of the longer value it held before.
//
// An attribute already holding the wanted value is left alone: writing
// power/control on a device carrying a job is not free, and there is no reason
// to spend it saying what is already true.
func writeSysfs(path, value string) error {
	if current, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(current)) == value {
		return nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		// An attribute this device does not have is not a failure to apply
		// anything: USB interfaces sit in the same directory as the devices
		// they belong to and carry a different set of knobs.
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer file.Close()
	if _, err := file.WriteString(value); err != nil {
		return err
	}
	return file.Close()
}
