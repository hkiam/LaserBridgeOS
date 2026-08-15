package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// The last resort, and only that.
//
// Everything else in this appliance recovers from something specific.
// supervise-daemon restarts a process that exits. BridgeWatch notices a GRBL
// bridge that is running but no longer answering, and restarts it - cheaply,
// without a reboot, keeping the rest of the system up. Both of those need the
// kernel to still be scheduling userspace.
//
// Nothing covered the case where it is not: a lockup, a driver spinning with
// interrupts off, a livelock under memory pressure. The appliance simply stops,
// with whatever the laser was doing left as it was, and ADR 0013's whole
// argument - that a beam must not be on while nothing moves - has no mechanism
// behind it at that point. A hardware watchdog is the only thing that acts when
// the software cannot, and on the way back through the reboot the USB bus is
// re-enumerated, which toggles DTR and resets an Arduino-based controller. That
// is a soft reset, and a soft reset switches the output off.
//
// What it deliberately does not do is judge. This process pets the watchdog for
// as long as it is being scheduled, and nothing else is required of the system.
// An earlier design tied the keepalive to a liveness check - the web backend
// answering, the bridge responding - and that is a worse trade than it sounds:
// the web service can be stopped from its own interface, and a watchdog whose
// false positive costs a reboot in the middle of a cut must have no plausible
// false positives. Hung processes are one layer up, where restarting one costs
// a connection instead of a job.
type Watchdog struct {
	// Path is the device; empty means /dev/watchdog.
	Path string
	// Timeout is how long the hardware waits before resetting the board. The
	// driver may round it or refuse it, and what it actually settled on is what
	// the interval is derived from.
	Timeout time.Duration
	Logger  *log.Logger
}

// ErrNoWatchdog reports that this machine has no watchdog device. It is a
// property of the hardware, not a failure of anything, and the caller decides
// what to say about it.
var ErrNoWatchdog = errors.New("no watchdog device")

const (
	// WDIOC_SETTIMEOUT from include/uapi/linux/watchdog.h: _IOWR('W', 6, int).
	// It is _IOWR rather than _IOW because the driver writes back the timeout
	// it was actually able to set.
	wdiocSetTimeout = 0xc0045706
	// magicClose is the byte that lets the watchdog be stopped by closing the
	// device. Anything else written pets it instead - which is why the pet
	// below writes a digit and not, say, a newline typed from a shell.
	magicClose = "V"
)

func (w *Watchdog) path() string {
	if w.Path != "" {
		return w.Path
	}
	return "/dev/watchdog"
}

func (w *Watchdog) timeout() time.Duration {
	if w.Timeout > 0 {
		return w.Timeout
	}
	// Long enough that a busy appliance under a large update never trips it,
	// short enough that a frozen one is not left with a live laser for a
	// minute.
	return 30 * time.Second
}

func (w *Watchdog) logf(format string, args ...any) {
	if w.Logger != nil {
		w.Logger.Printf(format, args...)
	}
}

// Run holds the device open and pets it until the context is cancelled.
func (w *Watchdog) Run(ctx context.Context) error {
	device, err := os.OpenFile(w.path(), os.O_WRONLY, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w at %s", ErrNoWatchdog, w.path())
		}
		return err
	}
	defer device.Close()

	// Being killed to save memory would reboot the appliance, and an
	// out-of-memory situation the kernel survives is precisely one this must
	// not turn into a reset. The score is advisory and the write may fail on a
	// kernel without it; that is not worth failing over.
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte("-1000\n"), 0644); err != nil {
		w.logf("could not protect the watchdog from the out-of-memory killer: %v", err)
	}

	seconds := int(w.timeout().Seconds())
	granted, err := setWatchdogTimeout(device.Fd(), seconds)
	if err != nil {
		// Some drivers have a fixed period. Petting far more often than
		// necessary costs nothing, so carry on rather than give up the only
		// mechanism that survives a freeze.
		w.logf("could not set the watchdog timeout (%v); using whatever the driver has", err)
		granted = seconds
	}
	interval := time.Duration(granted) * time.Second / 3
	if interval < time.Second {
		interval = time.Second
	}
	w.logf("watchdog armed on %s: %ds timeout, petted every %s", w.path(), granted, interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Written before the close, so that stopping this service on
			// purpose - an update, a shutdown, an operator debugging - does not
			// reboot the appliance a timeout later. A kernel built with
			// CONFIG_WATCHDOG_NOWAYOUT ignores it and reboots anyway, which is
			// that kernel's stated policy and not something to work around.
			if _, err := device.WriteString(magicClose); err != nil {
				w.logf("could not disarm the watchdog: %v", err)
			}
			w.logf("watchdog disarmed")
			return device.Close()
		case <-ticker.C:
			if _, err := device.WriteString("1"); err != nil {
				// Nothing to fall back to: if this write keeps failing the
				// board resets, which is the correct outcome for a watchdog
				// that can no longer be reached.
				w.logf("could not pet the watchdog: %v", err)
			}
		}
	}
}

func setWatchdogTimeout(fd uintptr, seconds int) (int, error) {
	value := int32(seconds)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, wdiocSetTimeout, uintptr(unsafe.Pointer(&value))); errno != 0 {
		return 0, errno
	}
	return int(value), nil
}
