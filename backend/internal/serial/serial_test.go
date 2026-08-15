package serial

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// openPTY gives a real tty to configure. A pseudo-terminal has no modem
// control lines to observe, so what is checked here is that the settings the
// bridge asks for are the settings the kernel ends up holding.
func openPTY(t *testing.T) (master *os.File, devicePath string) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	t.Cleanup(func() { master.Close() })

	var unlock int32
	if err := ioctl(int(master.Fd()), syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		t.Fatalf("unlock pty: %v", err)
	}
	var number uint32
	if err := ioctl(int(master.Fd()), syscall.TIOCGPTN, unsafe.Pointer(&number)); err != nil {
		t.Fatalf("pty number: %v", err)
	}
	return master, fmt.Sprintf("/dev/pts/%d", number)
}

func settings(t *testing.T, port *Port) termios2 {
	t.Helper()
	var got termios2
	if err := ioctl(int(port.Fd()), tcgets2, unsafe.Pointer(&got)); err != nil {
		t.Fatalf("read back settings: %v", err)
	}
	return got
}

func TestPortKeepsDTRAcrossAClose(t *testing.T) {
	// The one setting that decides whether restarting the daemon resets the
	// machine. With HUPCL set the kernel lowers DTR on the last close, and the
	// next open produces the edge an Arduino resets on.
	_, devicePath := openPTY(t)
	port, err := Open(devicePath, 115200)
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()

	if got := settings(t, port); got.CFlag&syscall.HUPCL != 0 {
		t.Error("HUPCL is set; a daemon restart would reset the controller")
	}
}

func TestPortIsRawAtTheRequestedRate(t *testing.T) {
	_, devicePath := openPTY(t)
	port, err := Open(devicePath, 250000)
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	got := settings(t, port)

	// 250000 has no B constant, which is the whole reason for termios2.
	if got.ISpeed != 250000 || got.OSpeed != 250000 {
		t.Errorf("speed = %d/%d, want 250000", got.ISpeed, got.OSpeed)
	}
	if got.CFlag&cbaud != bother {
		t.Error("the kernel was not told to read the speed from ISpeed/OSpeed")
	}
	// Raw: nothing may translate, echo or interpret a byte on its way through.
	for name, flag := range map[string]uint32{"ICRNL": syscall.ICRNL, "INLCR": syscall.INLCR, "IXON": syscall.IXON} {
		if got.IFlag&flag != 0 {
			t.Errorf("%s is set; input would be translated", name)
		}
	}
	if got.OFlag&syscall.OPOST != 0 {
		t.Error("OPOST is set; output would be translated")
	}
	if got.LFlag&(syscall.ICANON|syscall.ECHO|syscall.ISIG) != 0 {
		t.Error("the port is in canonical or echoing mode")
	}
	if got.CFlag&crtscts != 0 {
		t.Error("hardware flow control is on; GRBL does not use it")
	}
	if got.Cc[syscall.VMIN] != 1 || got.Cc[syscall.VTIME] != 0 {
		t.Errorf("VMIN/VTIME = %d/%d, want 1/0 so a read returns as soon as a byte arrives",
			got.Cc[syscall.VMIN], got.Cc[syscall.VTIME])
	}
}

func TestOpenRejectsAnImpossibleRate(t *testing.T) {
	if _, err := Open("/dev/null", 0); err == nil {
		t.Error("a zero baud rate was accepted")
	}
}
