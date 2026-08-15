package runtime

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
	"github.com/laserbridgeos/laserbridgeos/backend/internal/proxy"
)

// BridgeWatch notices a GRBL bridge that is running but not working.
//
// The bridge supervises the machine, which makes it the one process whose
// hanging matters most - and the one thing it cannot notice about itself.
// Nothing else would either: OpenRC's supervisor restarts a process that
// exits, not one that deadlocks, and the listening socket stays open and
// accepting on a wedged daemon, so "the port answers" is not evidence of
// anything. From outside, one question settles it: does it still say what it
// is doing?
//
// This lives in the web backend because that is a process that already exists,
// already runs as root, and is not the one being watched. Restarting the
// bridge costs the client its connection, which is why it takes several
// consecutive failures and does not repeat quickly - and why it is only worth
// doing at all now that a restart no longer resets the controller.
type BridgeWatch struct {
	Store      *config.Store
	Runner     Runner
	SocketPath string
	Logger     *log.Logger

	// Interval is how often to ask, Timeout how long to wait for an answer,
	// Failures how many unanswered questions make a hang, and Cooldown the
	// shortest gap between two restarts.
	Interval time.Duration
	Timeout  time.Duration
	Failures int
	Cooldown time.Duration
}

func (w *BridgeWatch) interval() time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return 15 * time.Second
}

func (w *BridgeWatch) failures() int {
	if w.Failures > 0 {
		return w.Failures
	}
	return 3
}

func (w *BridgeWatch) cooldown() time.Duration {
	if w.Cooldown > 0 {
		return w.Cooldown
	}
	return 5 * time.Minute
}

func (w *BridgeWatch) logf(format string, args ...any) {
	if w.Logger != nil {
		w.Logger.Printf(format, args...)
	}
}

// Responsive reports whether the bridge answered just now. It is also what the
// status page asks, so a hung daemon is not reported as running.
func (w *BridgeWatch) Responsive() bool {
	_, err := proxy.ReadStatusWithin(w.SocketPath, w.Timeout)
	return err == nil
}

// Watch runs until the context is cancelled.
func (w *BridgeWatch) Watch(ctx context.Context) {
	ticker := time.NewTicker(w.interval())
	defer ticker.Stop()

	missed := 0
	var lastRestart time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		cfg, err := w.Store.Load()
		if err != nil || cfg.GRBL.Backend != config.BackendLaserbridged {
			missed = 0
			continue
		}
		if _, err := proxy.ReadStatusWithin(w.SocketPath, w.Timeout); err == nil {
			if missed > 0 {
				w.logf("the GRBL bridge is answering again")
			}
			missed = 0
			continue
		}

		missed++
		if missed < w.failures() {
			continue
		}
		// Deliberately not asking rc-service whether it is running first.
		//
		// That check was here, and on a real appliance it made things worse.
		// Stopping a wedged daemon leaves OpenRC's supervisor gone and the
		// frozen child orphaned, and the service marked as stopped - at which
		// point "it is not running, so this is not a hang" is true, and acting
		// on it means walking away from an appliance with no bridge at all.
		//
		// The configuration is the authority on what should be running. If it
		// names laserbridged, laserbridged is meant to be up, and restart
		// starts a stopped service as readily as it restarts a running one.
		// Someone debugging by hand who stops the service will find it started
		// again within a minute; the answer for them is to switch the backend,
		// which is one line and is what the setting is for.
		if time.Since(lastRestart) < w.cooldown() {
			w.logf("the GRBL bridge is still not answering, but it was restarted %s ago; waiting",
				time.Since(lastRestart).Round(time.Second))
			continue
		}

		w.logf("the GRBL bridge has not answered %d times; restarting it", missed)
		if out, err := w.Runner.Run("rc-service", "laserbridged", "restart"); err != nil {
			w.logf("restarting the GRBL bridge failed: %s", string(out))
		}
		// A restart that ends with the service stopped is the failure that
		// matters most, and it is the one OpenRC produces when it cannot stop
		// a frozen child. Say so, and try to start it.
		if out, err := w.Runner.Run("rc-service", "laserbridged", "status"); err != nil {
			w.logf("the GRBL bridge is still not running after a restart (%s); starting it",
				strings.TrimSpace(string(out)))
			if out, err := w.Runner.Run("rc-service", "laserbridged", "start"); err != nil {
				w.logf("starting the GRBL bridge failed: %s", strings.TrimSpace(string(out)))
			}
		}
		lastRestart = time.Now()
		missed = 0
	}
}
