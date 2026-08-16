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
	"context"
	"errors"
	"fmt"
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
	// spindleStop is GRBL 1.1's spindle-stop override. It acts only in the
	// HOLD state and it TOGGLES: sending it to a controller whose output is
	// already stopped switches the laser back on. It is only ever sent here on
	// positive evidence that the beam is on.
	spindleStop = byte(0x9E)
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
	// ProbeIdentify bounds how long the opening questions wait for the
	// controller to give itself away. It is a setting rather than a constant
	// for the tests' sake: a bare pseudo-terminal never answers, so every test
	// that uses one waits out this budget before its client is served, and at
	// the production value that doubled the time the suite takes.
	ProbeIdentify time.Duration
	// BeamConfirm bounds how long the bridge waits for the controller to say
	// what its outputs are doing, after having asked it to change them.
	//
	// It is a budget of its own because it is a different question with a
	// different answer rate. Whether the axes have stopped can be read from
	// every status report; whether the output is on appears only in the Ov:
	// block, which GRBL prints every tenth report when idle and every
	// twentieth while moving. Sharing HoldSettle's three seconds meant asking
	// twenty times at 150ms and giving up at exactly the twentieth report -
	// the confirmation the whole escalation in ADR 0013 rests on was decided
	// by jitter.
	BeamConfirm time.Duration
	// SilenceAfter is how long a controller that had been answering may say
	// nothing before the bridge reports it as silent.
	SilenceAfter time.Duration
	// ClientWriteTimeout bounds how long a single write towards the client may
	// take before the client is treated as dead.
	ClientWriteTimeout time.Duration
	// JournalPath is where notable events are kept across a reboot. Empty
	// keeps them in memory only.
	JournalPath string
	// MonitorPort serves a read-only copy of the traffic. Zero switches it off.
	MonitorPort int
	// BeamGrace is how long the laser may be on with nobody attached before
	// the bridge switches it off itself.
	BeamGrace time.Duration
	// StationaryBeam is how long the laser may be on without the machine
	// moving. Zero switches the check off. This one has to be generous:
	// piercing thick material is a stationary burn on purpose.
	StationaryBeam time.Duration
	// IdlePoll is how long the appliance waits to hear from the controller
	// before asking itself. GRBL only speaks when spoken to, and a watchdog
	// reading a status report that stopped updating is worse than none.
	IdlePoll time.Duration
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
	// Gibberish is set when the port is open and answering with something that
	// is not GRBL - almost always the wrong baud rate.
	Gibberish bool `json:"gibberish"`
	// Monitors is how many read-only watchers are attached.
	Monitors int `json:"monitors"`
	// Job is the work in front of the machine: the one running, or the last
	// one and why it ended.
	Job Job `json:"job"`
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
	// droppedByUs is the connection the bridge closed itself while taking the
	// machine over, so the disconnect it caused is not mistaken for a client
	// walking away. Held as the connection rather than a flag: a client that
	// leaves of its own accord in the same moment must still be handled.
	droppedByUs net.Conn

	mu               sync.Mutex
	state            State
	devicePath       string
	clientAddr       string
	connectedSince   int64
	lastError        string
	lastIntervention string
	// ctx is cancelled when the daemon is stopping. Held on the bridge rather
	// than threaded through every call because the goroutines that need it
	// outlive the call that started them.
	ctx context.Context

	// observer reads along with the controller's output. It is fed a copy of
	// what is forwarded and can therefore never change it.
	observer *grbl.Observer
	// journal remembers what went wrong; sent remembers the last few lines the
	// client sent, so an error can be shown next to the line that caused it.
	journal  *Journal
	sent     *lineTail
	monitors *monitors

	// interveneMu makes sure only one thing is commanding the machine at a
	// time. The disconnect handler and the beam watchdog can both decide to
	// act, and two escalations interleaving would send a spindle-stop toggle
	// into the middle of another one's accounting - which would switch the
	// laser back on.
	interveneMu sync.Mutex
	// portOpenedAt is when the current serial port was opened, which is what
	// makes "has never said anything" a fault rather than an absence.
	portOpenedAt time.Time

	// probed is closed once the controller has been asked what it is, or once
	// it is clear that it cannot be. No client is served before then - see
	// probeController for why that gate has to exist.
	probed    chan struct{}
	probeOnce sync.Once

	// jobs is the appliance's notion of the work in front of the machine, as
	// opposed to what the machine is doing this instant.
	jobs jobTracker
	// lines counts newline-terminated lines the client has sent. A sender
	// counts the same thing, which is what makes it the useful unit.
	lines atomic.Uint64

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
	if config.ProbeIdentify <= 0 {
		config.ProbeIdentify = probeIdentify
	}
	if config.BeamConfirm <= 0 {
		// Fifty reports at the interval below: two and a half times the worst
		// documented cadence, so a missed confirmation means the controller
		// really did not say, rather than that nobody waited long enough.
		config.BeamConfirm = 5 * time.Second
	}
	if config.OnDisconnect == "" {
		config.OnDisconnect = DisconnectReset
	}
	if config.SilenceAfter <= 0 {
		config.SilenceAfter = 10 * time.Second
	}
	if config.ClientWriteTimeout <= 0 {
		config.ClientWriteTimeout = 5 * time.Second
	}
	if config.BeamGrace <= 0 {
		config.BeamGrace = time.Second
	}
	if config.IdlePoll <= 0 {
		config.IdlePoll = time.Second
	}
	if config.StationaryBeam < 0 {
		config.StationaryBeam = 0
	}
	bridge := &Bridge{
		config:   config,
		logger:   logger,
		probed:   make(chan struct{}),
		state:    StateStopped,
		observer: grbl.NewObserver(),
		journal:  NewJournal(config.JournalPath),
		sent:     newLineTail(),
		monitors: newMonitors(config.ClientWriteTimeout),
	}
	bridge.observer.OnReport = bridge.recordReport
	return bridge
}

// recordReport turns a notable line from the controller into a journal entry.
func (b *Bridge) recordReport(report grbl.Report, machine grbl.Machine) {
	event := Event{Kind: report.Kind, Text: report.Text, Code: report.Code, State: machine.State}
	if machine.HasPosition {
		position := machine.Position
		event.Position = &position
	}
	switch report.Kind {
	case "setting":
		// A $$ dump is thirty-odd lines and none of them are news, except the
		// one that decides whether a feed hold switches the beam off.
		if report.Setting == grbl.LaserMode {
			b.logf("laser mode ($32) is %s", report.SettingValue)
			if machine.LaserMode == grbl.SettingOff {
				b.journal.Add(Event{
					Kind: "fault",
					Text: "laser mode ($32) is off: this controller treats the laser as a spindle, " +
						"and a feed hold does not switch a spindle off",
					State: machine.State,
				})
			}
		}
		return
	case "ok":
		// Not worth recording, but it answers for a line, and the count is
		// what makes the next error attributable.
		b.sent.Acknowledge()
		return
	case "error":
		// GRBL replies in order, one per line accepted, so the line this
		// answered is the oldest one still outstanding.
		event.Line = b.sent.Acknowledge()
		event.Context = b.sent.Lines()
	case "alarm":
		// An alarm is not a reply to a line and answers for nothing, so the
		// queue is left alone; the recent lines are still worth having.
		event.Context = b.sent.Lines()
	case "welcome":
		event.Kind = "reset"
		event.Text = "the controller restarted: " + report.Text
		// A reset empties the controller's buffer, so every line still
		// outstanding was answered for by nobody.
		b.sent.Forget()
	}
	b.journal.Add(event)
	if report.Kind == "alarm" || report.Kind == "error" {
		b.logf("controller said %s", report.Text)
	}
}

// Journal is the record of what has gone wrong.
func (b *Bridge) Journal() *Journal { return b.journal }

func (b *Bridge) note(kind, text string) {
	machine := b.observer.Machine()
	b.journal.Add(Event{Kind: kind, Text: text, State: machine.State})
}

func (b *Bridge) Status() Status {
	// Read outside the lock: controllerSilent takes b.mu itself, and one
	// definition of silence is worth more than saving a lock acquisition.
	silent := b.controllerSilent()
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
		ControllerSilent: b.devicePath != "" && silent,
		Gibberish:        b.devicePath != "" && b.observer.Gibberish(),
		Monitors:         b.monitors.count(),
		Job:              b.jobs.current(),
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

// Run serves until the context is cancelled.
func (b *Bridge) Run(ctx context.Context, ready chan<- struct{}) error {
	b.mu.Lock()
	b.ctx = ctx
	b.mu.Unlock()
	listener, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(b.config.Port)))
	if err != nil {
		b.setState(StateFault)
		return err
	}
	defer listener.Close()

	deviceFinished := make(chan struct{})
	go b.serveDevice(ctx, deviceFinished)
	go b.watch(ctx)
	go b.serveMonitor(ctx)

	b.setState(StateListening)
	b.logf("listening on %s", listener.Addr())
	if ready != nil {
		close(ready)
	}

	go func() {
		<-ctx.Done()
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
			case <-ctx.Done():
			default:
				if !errors.Is(err, net.ErrClosed) {
					b.setError(err)
					continue
				}
			}
			b.DisconnectClient()
			<-deviceFinished
			// Last, so that anything the shutdown itself recorded is on disk
			// before the process leaves.
			b.journal.Close()
			b.setState(StateStopped)
			return nil
		}
		// Each connection gets its own goroutine. Handling it on the accept
		// loop would keep that loop busy for as long as a client is
		// connected, and a second client could never arrive to take over -
		// which is exactly what kick_old_user asks for.
		//
		// It waits, briefly, for the opening questions to the controller to be
		// finished. The connection is already accepted at the TCP level, so a
		// client sees a socket that is quiet for a moment rather than a refusal
		// - and the settings dump those questions produce goes nowhere, because
		// b.client is still nil while it arrives.
		go func(conn net.Conn) {
			select {
			case <-b.probed:
			case <-time.After(b.probeGrace()):
			}
			b.serveClient(conn)
		}(conn)
	}
}

// serveDevice keeps the serial port open and forwards whatever the controller
// says to whoever is attached. It reads even with no client: the kernel
// buffer would otherwise fill with GRBL's unsolicited output and the next
// client would be greeted by a backlog of stale messages.
func (b *Bridge) serveDevice(ctx context.Context, finished chan<- struct{}) {
	defer close(finished)
	buffer := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			b.closePort()
			return
		default:
		}

		port := b.openPort(ctx)
		if port == nil {
			b.closePort()
			return
		}
		// Only ever on the first port this daemon opens, and concurrently with
		// the read loop below, which is what feeds the observer the answers.
		//
		// Not on a re-open after the adapter came back: the accept gate is a
		// one-time thing, so by then a client can be served at any moment and a
		// queued command would land in the middle of its line accounting. The
		// cost is that a replugged adapter leaves $32 unknown again until a
		// client asks for it, which is where it was before any of this.
		select {
		case <-b.probed:
		default:
			go b.probeController(port)
		}

		for {
			n, err := port.Read(buffer)
			if n > 0 {
				b.tx.Add(uint64(n))
				// The client is served first. Reading along must never be a
				// reason for G-code acknowledgements to arrive later.
				b.writeToClient(buffer[:n])
				b.observer.Write(buffer[:n])
				b.monitors.send('<', buffer[:n])
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
		case <-ctx.Done():
			return
		default:
		}
		// The adapter went away mid-stream. Drop the client with it: a client
		// left connected to nothing would keep sending G-code into a void and
		// believe it was cutting.
		b.logf("serial port lost")
		b.note("device", "the serial port disappeared; the client was dropped with it")
		b.DisconnectClient()
	}
}

// openPort waits for the serial device to exist, then opens it.
func (b *Bridge) openPort(ctx context.Context) *serial.Port {
	announced := false
	for {
		port, err := serial.Open(serial.Resolve(b.config.Device), b.config.Baudrate)
		if err == nil {
			// Whatever the reading described, it described a controller that is
			// no longer on the other end of this file descriptor.
			b.observer.Reset()

			b.portMu.Lock()
			// Publishing the port and being told to stop are the same race,
			// and losing it hangs the daemon: the shutdown path closes the
			// port to unblock the reader below, and a port that was not yet
			// published when it ran never gets closed at all. Both sides take
			// this lock, so checking here closes the window - either the
			// shutdown finds the port, or this finds the shutdown.
			select {
			case <-ctx.Done():
				b.portMu.Unlock()
				_ = port.Close()
				return nil
			default:
			}
			b.port = port
			b.portMu.Unlock()
			b.mu.Lock()
			b.devicePath = port.Path()
			b.portOpenedAt = time.Now()
			b.mu.Unlock()
			b.logf("serial port %s open at %d baud", port.Path(), b.config.Baudrate)
			b.note("device", "opened "+port.Path()+" at "+strconv.Itoa(b.config.Baudrate)+" baud")
			if b.currentClient() == nil {
				b.setState(StateListening)
			}
			return port
		}
		if !announced {
			b.setState(StateWaitingForDevice)
			b.setError(err)
			announced = true
			// Nothing to ask and nobody to ask it of, so a client that
			// connects now must not be held waiting for questions that will
			// never be put.
			b.finishProbe()
		}
		select {
		case <-ctx.Done():
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

// writeToClient forwards the controller's output to whoever is attached.
//
// The deadline is not a nicety. Without one, a client whose TCP window has
// closed - a laptop on a stalled Wi-Fi link, still connected as far as the
// kernel is concerned - blocks this write indefinitely, and with it the only
// goroutine reading the serial port. The controller keeps talking into a
// 4 KB kernel buffer that nobody is draining, and its output is lost. A
// client that cannot take a status report within five seconds is in no state
// to run a job, so it is dropped and the machine keeps being read.
func (b *Bridge) writeToClient(data []byte) {
	client := b.currentClient()
	if client == nil {
		return
	}
	_ = client.SetWriteDeadline(time.Now().Add(b.config.ClientWriteTimeout))
	if _, err := client.Write(data); err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			b.setError(fmt.Errorf("client %s stopped accepting data; dropping it", client.RemoteAddr()))
		}
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
		//
		// The intervals matter as much as switching it on. The defaults probe
		// nine times at the configured period, which at any sane period means
		// minutes before a dead client is noticed - and the whole disconnect
		// response is waiting behind that notice. Five seconds idle, three
		// probes two seconds apart, so a laptop that drops off the network is
		// gone in about eleven seconds on a link that is otherwise quiet.
		_ = tcp.SetKeepAliveConfig(net.KeepAliveConfig{
			Enable:   true,
			Idle:     5 * time.Second,
			Interval: 2 * time.Second,
			Count:    3,
		})
	}

	b.clientMu.Lock()
	if previous := b.client; previous != nil {
		if !b.config.KickOldClient {
			b.rejected.Add(1)
			b.logf("refusing %s: %s still connected", conn.RemoteAddr(), previous.RemoteAddr())
			b.note("client", "refused "+conn.RemoteAddr().String()+"; "+previous.RemoteAddr().String()+" still connected")
			b.clientMu.Unlock()
			_ = conn.Close()
			return
		}
		b.logf("closing %s to make room for %s", previous.RemoteAddr(), conn.RemoteAddr())
		b.note("client", "closed "+previous.RemoteAddr().String()+" to make room for "+conn.RemoteAddr().String())
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
	b.note("client", "connected: "+conn.RemoteAddr().String())
	// Whatever the last client left in the controller's buffer is not this
	// one's, and counting replies against its lines would misattribute them.
	b.sent.Forget()

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
		b.note("client", "disconnected: "+conn.RemoteAddr().String())
		// A job whose sender has gone is over whatever the machine does with
		// what is left in its buffer.
		if note := b.jobs.end("ended with the client", time.Now()); note != "" {
			b.note("job", note)
		}
		// After the bookkeeping, not before: an intervention can take a few
		// seconds, and the status should already read LISTENING rather than
		// claim a client that has gone.
		b.onClientGone(conn)
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
			// Recorded before it is sent, not after. The controller's replies
			// are read on another goroutine, and nothing orders that goroutine
			// against this one: a controller that answers quickly enough can
			// have its reply processed before this line was written down, and
			// a reply for a line nobody sent throws the attribution off for
			// the rest of the connection. Recording first removes the
			// possibility. The cost is a byte loop ahead of a write that a
			// 115200-baud wire takes a thousand times longer to carry.
			//
			// This is a correctness argument, not a diagnosed failure - the
			// window was never observed to be hit.
			for _, c := range buffer[:n] {
				if c == '\n' {
					b.lines.Add(1)
				}
			}
			b.sent.Write(buffer[:n])
			b.monitors.send('>', buffer[:n])
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
