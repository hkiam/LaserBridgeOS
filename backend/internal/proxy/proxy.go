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

	"github.com/laserbridgeos/laserbridgeos/backend/internal/grbl"
	"github.com/laserbridgeos/laserbridgeos/backend/internal/serial"
)

// What to do when the client disappears while the machine is moving. The
// values are the ones the appliance configuration uses; a test asserts they
// have not drifted apart.
const (
	DisconnectNone  = "none"
	DisconnectHold  = "hold"
	DisconnectReset = "reset"
)

// GRBL's real-time control bytes. They are acted on the moment the controller
// reads them, ahead of anything queued, which is what makes them usable for
// stopping a machine whose planner buffer is full of work.
const (
	feedHold  = byte('!')
	statusReq = byte('?')
	softReset = byte(0x18)
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
	// OnDisconnect is "none", "hold" or "reset".
	OnDisconnect string
	// HoldSettle bounds how long the bridge waits for the machine to come to
	// rest after a feed hold before it sends a soft reset.
	HoldSettle time.Duration
	// SilenceAfter is how long a controller that had been answering may say
	// nothing before the bridge reports it as silent.
	SilenceAfter time.Duration
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
	// Machine is what the controller last said about itself. It comes from
	// reading along with the stream, never from asking on a client's behalf.
	Machine grbl.Machine `json:"machine"`
	// LastIntervention describes what the bridge last did on its own accord,
	// so an operator finding a paused machine can see why it paused.
	LastIntervention string `json:"last_intervention,omitempty"`
	// ControllerSilent is set when the controller answered once and then
	// stopped while a client was attached. It is a reading, not a verdict.
	ControllerSilent bool `json:"controller_silent"`
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

	mu               sync.Mutex
	state            State
	devicePath       string
	clientAddr       string
	connectedSince   int64
	lastError        string
	lastIntervention string
	done             <-chan struct{}

	// observer reads along with the controller's output. It is fed a copy of
	// what is forwarded and can therefore never change it.
	observer *grbl.Observer

	rx       atomic.Uint64
	tx       atomic.Uint64
	rejected atomic.Uint64
	handled  atomic.Uint64
}

func New(config Config, logger *log.Logger) *Bridge {
	if config.DeviceRetry <= 0 {
		config.DeviceRetry = 2 * time.Second
	}
	if config.HoldSettle <= 0 {
		config.HoldSettle = 3 * time.Second
	}
	if config.OnDisconnect == "" {
		config.OnDisconnect = DisconnectHold
	}
	if config.SilenceAfter <= 0 {
		config.SilenceAfter = 10 * time.Second
	}
	return &Bridge{config: config, logger: logger, state: StateStopped, observer: grbl.NewObserver()}
}

func (b *Bridge) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Status{
		State:            b.state,
		Device:           b.devicePath,
		ConfiguredPort:   b.config.Port,
		Client:           b.clientAddr,
		ConnectedSince:   b.connectedSince,
		RxBytes:          b.rx.Load(),
		TxBytes:          b.tx.Load(),
		LastError:        b.lastError,
		ClientsRejected:  b.rejected.Load(),
		Machine:          b.observer.Machine(),
		LastIntervention: b.lastIntervention,
		ControllerSilent: b.observer.Silent(b.config.SilenceAfter) && b.clientAddr != "",
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
	b.mu.Lock()
	b.done = done
	b.mu.Unlock()
	listener, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(b.config.Port)))
	if err != nil {
		b.setState(StateFault)
		return err
	}
	defer listener.Close()

	deviceFinished := make(chan struct{})
	go b.serveDevice(done, deviceFinished)
	go b.watch(done)

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

// watch notices a controller that has stopped answering and says so. It does
// not act on it.
//
// GRBL speaks when spoken to: a quiet link usually means the client has
// nothing to ask, not that anything is wrong. Only the client knows whether it
// is mid-job, so a bridge that held the machine on silence would interrupt
// perfectly good work on a guess. What it can do is report the observation and
// let the operator - who can see the machine - decide.
func (b *Bridge) watch(done <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	reported := false
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}
		silent := b.observer.Silent(b.config.SilenceAfter) && b.currentClient() != nil
		if silent && !reported {
			b.logf("the controller has not answered for over %s while a client is connected", b.config.SilenceAfter)
			reported = true
		}
		if !silent {
			reported = false
		}
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
				// The client is served first. Reading along must never be a
				// reason for G-code acknowledgements to arrive later.
				b.writeToClient(buffer[:n])
				b.observer.Write(buffer[:n])
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
			// Whatever the reading described, it described a controller that is
			// no longer on the other end of this file descriptor.
			b.observer.Reset()
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
		b.logf("client %s disconnected", conn.RemoteAddr())
		// After the bookkeeping, not before: an intervention can take a few
		// seconds, and the status should already read LISTENING rather than
		// claim a client that has gone.
		b.onClientGone()
		return
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

// onClientGone decides what a machine left to itself should do.
//
// A laptop that closes its lid mid-job leaves a controller working through
// whatever is still in its planner buffer, with nobody watching. Sending a
// feed hold pauses motion and switches the beam off while keeping the
// position, so the job can be resumed once the client is back. That is a
// convenience for an unattended machine and nothing more: it depends on this
// daemon running, the serial link working and the controller answering, so it
// is not a substitute for a hardware emergency stop and must never be
// presented as one.
func (b *Bridge) onClientGone() {
	// Counted whatever the outcome, including "nothing to do": it is what lets
	// a test wait for the decision to have been made rather than sleep and
	// hope.
	defer b.handled.Add(1)

	action := b.config.OnDisconnect
	if action == DisconnectNone {
		return
	}
	// A daemon being stopped is not a client walking away. Somebody asked for
	// this - a service restart after a settings change, say - so the machine
	// is not unattended and the job should not be interrupted.
	if b.aborted() {
		return
	}
	machine := b.observer.Machine()
	if !machine.State.Moving() {
		// Nothing is moving, so there is nothing to stop. Sending a hold to an
		// idle machine is harmless but would leave it in a state the next
		// client has to clear before it can do anything.
		return
	}
	port := b.currentPort()
	if port == nil {
		return
	}

	b.logf("client left while machine was %s; sending feed hold", machine.State)
	if _, err := port.Write([]byte{feedHold}); err != nil {
		b.setError(err)
		return
	}
	b.noteIntervention("feed hold after the client disconnected while " + string(machine.State))
	if action != DisconnectReset {
		return
	}

	// A soft reset while the machine is still decelerating loses the position
	// and lands the controller in an alarm, so wait for the hold to take
	// effect first. The machine is asked how it is doing - a status request
	// changes nothing and is the one thing safe to send unbidden.
	if !b.waitUntilStill(port) {
		b.logf("machine did not come to rest within %s; sending soft reset anyway", b.config.HoldSettle)
	}
	if b.aborted() {
		return
	}
	if _, err := port.Write([]byte{softReset}); err != nil {
		b.setError(err)
		return
	}
	b.noteIntervention("soft reset after the client disconnected mid-job")
	b.logf("soft reset sent")
}

// waitUntilStill polls until the controller reports its axes have stopped.
//
// It asks for "at rest", not "not moving": a machine on feed hold is standing
// still with a job still in its buffer, which is exactly the state the hold
// was meant to produce and exactly when the reset may safely follow.
func (b *Bridge) waitUntilStill(port *serial.Port) bool {
	deadline := time.NewTimer(b.config.HoldSettle)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := port.Write([]byte{statusReq}); err != nil {
			return false
		}
		select {
		case <-ticker.C:
			if b.observer.Machine().State.AtRest() {
				return true
			}
			// A newcomer takes charge; the bridge stops acting on its own.
			if b.currentClient() != nil || b.aborted() {
				return false
			}
		case <-deadline.C:
			return false
		}
	}
}

// aborted reports whether the daemon is shutting down, so an intervention in
// progress does not hold up a service restart.
func (b *Bridge) aborted() bool {
	b.mu.Lock()
	done := b.done
	b.mu.Unlock()
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func (b *Bridge) noteIntervention(what string) {
	b.mu.Lock()
	b.lastIntervention = what
	b.mu.Unlock()
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
