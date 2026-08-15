// Package serial opens a serial port for raw 8N1 traffic.
//
// It talks to the kernel through the standard library's syscall package
// rather than a serial library, because the appliance builds without
// dependencies and a serial port is three ioctls.
package serial

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// termios2 is the modern terminal settings structure. The older termios only
// carries a baud rate as one of a fixed set of B... constants, and the
// appliance permits rates such as 250000 that have no constant. termios2
// takes the number itself, so every rate the configuration accepts can
// actually be set.
type termios2 struct {
	IFlag  uint32
	OFlag  uint32
	CFlag  uint32
	LFlag  uint32
	Line   uint8
	Cc     [19]uint8
	ISpeed uint32
	OSpeed uint32
}

const (
	tcgets2 = 0x802c542a
	tcsets2 = 0x402c542b
	// BOTHER tells the kernel to read the speed from ISpeed/OSpeed instead
	// of the CBAUD bits.
	bother = 0x1000
	cbaud  = 0x100f
	// Hardware flow control. The standard library does not define it, and
	// GRBL does not use it, so it is cleared explicitly.
	crtscts = 0x80000000
)

// Port is an open serial port.
type Port struct {
	*os.File
	path string
}

// Path is the device the port was opened from, which may be a stable
// /dev/serial/by-id/ name rather than the one that was configured.
func (p *Port) Path() string { return p.path }

// Open configures the port for 8N1, no flow control, and raw byte traffic:
// nothing is translated, buffered by line, or interpreted, because a GRBL
// stream carries control bytes such as 0x18 that must arrive unchanged.
func Open(device string, baud int) (*Port, error) {
	if baud <= 0 {
		return nil, fmt.Errorf("invalid baud rate %d", baud)
	}
	// O_NOCTTY: the port must not become this process's controlling
	// terminal. O_NONBLOCK: opening must not wait for carrier detect, which
	// some adapters never assert.
	fd, err := syscall.Open(device, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", device, err)
	}

	// One process at a time. The kernel refuses a second exclusive open,
	// which is a stronger guarantee than the appliance's own accounting.
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCEXCL, 0)

	if err := configure(fd, baud); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("configure %s: %w", device, err)
	}
	return &Port{File: os.NewFile(uintptr(fd), device), path: device}, nil
}

func configure(fd int, baud int) error {
	var t termios2
	if err := ioctl(fd, tcgets2, unsafe.Pointer(&t)); err != nil {
		return fmt.Errorf("read terminal settings: %w", err)
	}

	// Raw input: no parity checks, no CR/LF translation, no flow control,
	// no signal generation. Every byte reaches the other side as it arrived.
	t.IFlag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON | syscall.IXOFF | syscall.IXANY
	t.OFlag &^= syscall.OPOST
	t.LFlag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN

	// 8 data bits, no parity, one stop bit. CLOCAL ignores modem control
	// lines, CREAD enables the receiver, CRTSCTS off means no hardware flow
	// control - GRBL does not use it.
	t.CFlag &^= syscall.CSIZE | syscall.PARENB | syscall.CSTOPB | crtscts
	t.CFlag |= syscall.CS8 | syscall.CLOCAL | syscall.CREAD

	// HUPCL off is the important one, and it is about DTR rather than about
	// terminals.
	//
	// An Arduino-based GRBL controller resets when DTR is asserted, because
	// the auto-reset circuit differentiates that edge onto the RESET pin.
	// Opening the port asserts DTR - but only produces an edge if DTR was not
	// asserted already. With HUPCL set, the kernel lowers DTR when the last
	// process closes the port, so the next open produces exactly that edge:
	// every restart of this daemon would reset the machine, mid-job included.
	// With HUPCL clear, DTR stays up across a restart and the controller never
	// notices we were gone.
	t.CFlag &^= syscall.HUPCL

	t.CFlag &^= cbaud
	t.CFlag |= bother
	t.ISpeed = uint32(baud)
	t.OSpeed = uint32(baud)

	// A read returns as soon as one byte is there. Latency matters more than
	// batching for an interactive control stream.
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0

	if err := ioctl(fd, tcsets2, unsafe.Pointer(&t)); err != nil {
		return fmt.Errorf("apply terminal settings: %w", err)
	}
	return nil
}

func ioctl(fd int, request uintptr, argument unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), request, uintptr(argument)); errno != 0 {
		return errno
	}
	return nil
}

// Resolve prefers a stable /dev/serial/by-id/ name for the same device.
//
// A USB adapter is /dev/ttyUSB0 until something else claims that number
// first, at which point a configuration naming it points at the wrong
// hardware. The by-id name is derived from the adapter itself and survives
// re-enumeration, so it is the better thing to open and to report.
func Resolve(device string) string {
	if strings.HasPrefix(device, "/dev/serial/by-id/") {
		return device
	}
	target, err := filepath.EvalSymlinks(device)
	if err != nil {
		return device
	}
	links, err := filepath.Glob("/dev/serial/by-id/*")
	if err != nil {
		return device
	}
	for _, link := range links {
		if resolved, err := filepath.EvalSymlinks(link); err == nil && resolved == target {
			return link
		}
	}
	return device
}
