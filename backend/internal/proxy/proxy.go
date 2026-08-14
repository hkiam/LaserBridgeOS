// Package proxy carries bytes between a TCP client and a serial port.
//
// It is deliberately transparent: whatever arrives on one side leaves the
// other unchanged and unbuffered by line. A GRBL stream carries control bytes
// - 0x18 soft reset, ! feed hold, ~ resume, ? status - that mean nothing to
// the bridge and everything to the controller, so the bridge does not look at
// them. Knowing what they mean comes later; carrying them faithfully comes
// first.
//
// The serial port is opened once and held for the life of the daemon, not per
// client. Opening a USB serial adapter toggles DTR, which resets an
// Arduino-based GRBL controller - so a bridge that reopened the port on every
// reconnect would reset the machine each time LightBurn came back. Clients
// attach to a port that is already open.
package proxy

import (
	"errors"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/serial"
)

// State is what the bridge is currently doing. The web interface and the API
// read the same value, so there is one answer to "what is it doing" rather
// than one per caller.
type State string

const (
	StateStopped          State = "STOPPED"
	StateWaitingForDevice State = "WAITING_FOR_DEVICE"
	StateListening        State = "LISTENING"
	StateClientConnected  State = "CLIENT_CONNECTED"
	StateFault            State = "FAULT"
)

// Config is the subset of the appliance configuration the bridge needs.
type Config struct {
	Device        string
	Baudrate      int
	Port          int
	KickOldClient bool
	// DeviceRetry is how long to wait before looking again for a serial port
	// that is not there, so an adapter that is unplugged and plugged back in
	// is picked up without anyone restarting the service.
	DeviceRetry time.Duration
}

// Status is the snapshot handed out over the status socket.
type Status struct {
	State           State  `json:"state"`
	Device          string `json:"device,omitempty"`
	ConfiguredPort  int    `json:"tcp_port"`
	Client          string `json:"client,omitempty"`
	ConnectedSince  int64  `json:"connected_since,omitempty"`
	RxBytes         uint64 `json:"rx_bytes"`
	TxBytes         uint64 `json:"tx_bytes"`
	LastError       string `json:"last_error,omitempty"`
	ClientsRejected uint64 `json:"clients_rejected"`
}

// Bridge is the running proxy.
type Bridge struct {
	config Config
	logger *log.Logger

	// portMu guards the open serial port, which outlives any client.
	portMu sync.Mutex
	port   *serial.Port

	// clientMu guards which client currently owns the port.
	clientMu sync.Mutex
	client   net.Conn

	mu             sync.Mutex
	state          State
	devicePath     string
	clientAddr     string
	connectedSince int64
	lastError      string

	rx       atomic.Uint64
	tx       atomic.Uint64
	rejected atomic.Uint64
}

func New(config Config, logger *log.Logger) *Bridge {
	if config.DeviceRetry <= 0 {
		config.DeviceRetry = 2 * time.Second
	}
	return &Bridge{config: config, logger: logger, state: StateStopped}
}

func (b *Bridge) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Status{
		State:           b.state,
		Device:          b.devicePath,
		ConfiguredPort:  b.config.Port,
		Client:          b.clientAddr,
		ConnectedSince:  b.connectedSince,
		RxBytes:         b.rx.Load(),
		TxBytes:         b.tx.Load(),
		LastError:       b.lastError,
		ClientsRejected: b.rejected.Load(),
	}
}

func (b *Bridge) setState(state State) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != state {
		b.state = state
		b.logf("state %s", state)
	}
}

func (b *Bridge) setError(err error) {
	b.mu.Lock()
	b.lastError = err.Error()
	b.mu.Unlock()
	b.logf("%v", err)
}

func (b *Bridge) logf(format string, args ...any) {
	if b.logger != nil {
		b.logger.Printf(format, args...)
	}
}

// Run serves until done is closed.
func (b *Bridge) Run(done <-chan struct{}, ready chan<- struct{}) error {
	listener, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(b.config.Port)))
	if err != nil {
		b.setState(StateFault)
		return err
	}
	defer listener.Close()

	deviceFinished := make(chan struct{})
	go b.serveDevice(done, deviceFinished)

	b.setState(StateListening)
	b.logf("listening on %s", listener.Addr())
	if ready != nil {
		close(ready)
	}

	go func() {
		<-done
		// Closing the port is what unblocks the device reader, which is
		// otherwise parked in a read that never returns on its own.
		listener.Close()
		b.closePort()
		b.DisconnectClient()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-done:
			default:
				if !errors.Is(err, net.ErrClosed) {
					b.setError(err)
					continue
				}
			}
			b.DisconnectClient()
			<-deviceFinished
			b.setState(StateStopped)
			return nil
		}
		// Each connection gets its own goroutine. Handling it on the accept
		// loop would keep that loop busy for as long as a client is
		// connected, and a second client could never arrive to take over -
		// which is exactly what kick_old_user asks for.
		go b.serveClient(conn)
	}
}

// serveDevice keeps the serial port open and forwards whatever the controller
// says to whoever is attached. It reads even with no client: the kernel
// buffer would otherwise fill with GRBL's unsolicited output and the next
// client would be greeted by a backlog of stale messages.
func (b *Bridge) serveDevice(done <-chan struct{}, finished chan<- struct{}) {
	defer close(finished)
	buffer := make([]byte, 4096)
	for {
		select {
		case <-done:
			b.closePort()
			return
		default:
		}

		port := b.openPort(done)
		if port == nil {
			b.closePort()
			return
		}

		for {
			n, err := port.Read(buffer)
			if n > 0 {
				b.tx.Add(uint64(n))
				b.writeToClient(buffer[:n])
			}
			if err != nil {
				if !isExpectedClose(err) {
					b.setError(err)
				}
				break
			}
		}

		b.closePort()
		select {
		case <-done:
			return
		default:
		}
		// The adapter went away mid-stream. Drop the client with it: a client
		// left connected to nothing would keep sending G-code into a void and
		// believe it was cutting.
		b.logf("serial port lost")
		b.DisconnectClient()
	}
}

// openPort waits for the serial device to exist, then opens it.
func (b *Bridge) openPort(done <-chan struct{}) *serial.Port {
	announced := false
	for {
		port, err := serial.Open(serial.Resolve(b.config.Device), b.config.Baudrate)
		if err == nil {
			b.portMu.Lock()
			b.port = port
			b.portMu.Unlock()
			b.mu.Lock()
			b.devicePath = port.Path()
			b.mu.Unlock()
			b.logf("serial port %s open at %d baud", port.Path(), b.config.Baudrate)
			if b.currentClient() == nil {
				b.setState(StateListening)
			}
			return port
		}
		if !announced {
			b.setState(StateWaitingForDevice)
			b.setError(err)
			announced = true
		}
		select {
		case <-done:
			return nil
		case <-time.After(b.config.DeviceRetry):
		}
	}
}

func (b *Bridge) closePort() {
	b.portMu.Lock()
	port := b.port
	b.port = nil
	b.portMu.Unlock()
	if port != nil {
		_ = port.Close()
	}
}

func (b *Bridge) currentPort() *serial.Port {
	b.portMu.Lock()
	defer b.portMu.Unlock()
	return b.port
}

func (b *Bridge) currentClient() net.Conn {
	b.clientMu.Lock()
	defer b.clientMu.Unlock()
	return b.client
}

func (b *Bridge) writeToClient(data []byte) {
	client := b.currentClient()
	if client == nil {
		return
	}
	if _, err := client.Write(data); err != nil {
		// The client is gone; its own goroutine notices and tidies up.
		_ = client.Close()
	}
}

// serveClient attaches one TCP connection to the serial port.
func (b *Bridge) serveClient(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		// Without keepalives a client that vanishes - laptop closed, Wi-Fi
		// gone - leaves the bridge holding a socket that never reports an
		// error, and the port stays claimed by a machine that is not there.
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(15 * time.Second)
	}

	b.clientMu.Lock()
	if previous := b.client; previous != nil {
		if !b.config.KickOldClient {
			b.rejected.Add(1)
			b.logf("refusing %s: %s still connected", conn.RemoteAddr(), previous.RemoteAddr())
			b.clientMu.Unlock()
			_ = conn.Close()
			return
		}
		b.logf("closing %s to make room for %s", previous.RemoteAddr(), conn.RemoteAddr())
		_ = previous.Close()
	}
	b.client = conn
	b.clientMu.Unlock()

	b.mu.Lock()
	b.clientAddr = conn.RemoteAddr().String()
	b.connectedSince = time.Now().Unix()
	b.mu.Unlock()
	b.setState(StateClientConnected)
	b.logf("client %s connected", conn.RemoteAddr())

	b.readFromClient(conn)

	_ = conn.Close()
	b.clientMu.Lock()
	// Only retire the status if nobody has taken over in the meantime,
	// otherwise this would erase the newcomer's details.
	mine := b.client == conn
	if mine {
		b.client = nil
	}
	b.clientMu.Unlock()
	if mine {
		b.mu.Lock()
		b.clientAddr = ""
		b.connectedSince = 0
		b.mu.Unlock()
		// The state has to move on whether or not a device is present,
		// otherwise it would keep claiming CLIENT_CONNECTED after the client
		// left simply because there was no serial port to fall back to.
		if b.currentPort() != nil {
			b.setState(StateListening)
		} else {
			b.setState(StateWaitingForDevice)
		}
	}
	b.logf("client %s disconnected", conn.RemoteAddr())
}

// readFromClient forwards G-code to the controller until the client goes away.
func (b *Bridge) readFromClient(conn net.Conn) {
	buffer := make([]byte, 4096)
	for {
		n, err := conn.Read(buffer)
		if n > 0 {
			b.rx.Add(uint64(n))
			port := b.currentPort()
			if port == nil {
				// Nothing to write to. Say so rather than pretending the
				// bytes were delivered to a controller.
				b.setError(errors.New("client sent data while no serial port is open"))
				return
			}
			if _, writeErr := port.Write(buffer[:n]); writeErr != nil {
				if !isExpectedClose(writeErr) {
					b.setError(writeErr)
				}
				return
			}
		}
		if err != nil {
			if !isExpectedClose(err) {
				b.setError(err)
			}
			return
		}
	}
}

// DisconnectClient drops the current client, if any.
func (b *Bridge) DisconnectClient() {
	b.clientMu.Lock()
	client := b.client
	b.clientMu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}

func isExpectedClose(err error) bool {
	return errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, os.ErrClosed)
}
