package api

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// newPTY gives the test a controller to speak as and a device path for the
// bridge to open. The proxy package has its own copy; duplicating twenty lines
// is cheaper than exporting test scaffolding from a package that has no other
// reason to.
func newPTY(t *testing.T) (controller *os.File, devicePath string) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	t.Cleanup(func() { master.Close() })

	var unlock int32
	if err := ptyIoctl(master.Fd(), syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		t.Skipf("unlock pty: %v", err)
	}
	var number uint32
	if err := ptyIoctl(master.Fd(), syscall.TIOCGPTN, unsafe.Pointer(&number)); err != nil {
		t.Skipf("pty number: %v", err)
	}
	return master, fmt.Sprintf("/dev/pts/%d", number)
}

func ptyIoctl(fd uintptr, request uintptr, argument unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(argument)); errno != 0 {
		return errno
	}
	return nil
}
