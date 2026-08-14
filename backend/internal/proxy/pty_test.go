package proxy

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// openPTY returns a connected pair: the controller side, which the test drives
// as if it were the GRBL board, and the path of the device side, which the
// bridge opens as if it were a serial port.
//
// A pseudo-terminal is the honest stand-in. It is a real tty, so the same
// termios calls apply and the same raw byte semantics hold - a test that
// passes here exercises the code that runs against an actual adapter.
func openPTY() (controller *os.File, devicePath string, err error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, "", err
	}

	var unlock int32
	if err := ioctlPtr(master.Fd(), syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("unlock pty: %w", err)
	}

	var number uint32
	if err := ioctlPtr(master.Fd(), syscall.TIOCGPTN, unsafe.Pointer(&number)); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("pty number: %w", err)
	}
	return master, fmt.Sprintf("/dev/pts/%d", number), nil
}

func ioctlPtr(fd uintptr, request uintptr, argument unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(argument)); errno != 0 {
		return errno
	}
	return nil
}
