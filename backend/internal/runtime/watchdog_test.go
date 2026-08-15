package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWatchdogPetsAndDisarms(t *testing.T) {
	// A regular file stands in for the device: it takes the writes, and the
	// ioctl that a real driver answers fails on it, which is the path a driver
	// with a fixed period also takes. What matters is the sequence - it is
	// petted while running, and the last thing written before the close is the
	// magic character that stops it. Without that, stopping this service on
	// purpose would reboot the appliance a timeout later.
	path := filepath.Join(t.TempDir(), "watchdog")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	dog := &Watchdog{Path: path, Timeout: 3 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dog.Run(ctx) }()

	time.Sleep(1300 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog did not stop when asked")
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "1") {
		t.Errorf("the watchdog was never petted: %q", written)
	}
	if !strings.HasSuffix(string(written), magicClose) {
		t.Errorf("written = %q, want it to end with the magic close character", written)
	}
}

func TestWatchdogSaysWhenThereIsNone(t *testing.T) {
	// Most virtual machines have no watchdog device. That is a fact about the
	// hardware and has to be reported as one, so the init script can say it
	// plainly instead of the appliance looking protected when it is not.
	dog := &Watchdog{Path: filepath.Join(t.TempDir(), "absent")}
	err := dog.Run(context.Background())
	if !errors.Is(err, ErrNoWatchdog) {
		t.Fatalf("Run() = %v, want ErrNoWatchdog", err)
	}
}
