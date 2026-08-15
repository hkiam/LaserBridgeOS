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
		ControllerSilent: b.devicePath != "" && b.silentLocked(),
		Gibberish:        b.devicePath != "" && b.observer.Gibberish(),
		Monitors:         b.monitors.count(),
	}
}

// silentLocked is controllerSilent for a caller that already holds b.mu.
func (b *Bridge) silentLocked() bool {
	if b.portOpenedAt.IsZero() {
		return false
	}
	if b.observer.Machine().LastReportUnix == 0 {
		return time.Since(b.portOpenedAt) > b.config.SilenceAfter
	}
	return b.observer.Silent(b.config.SilenceAfter)
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
	go b.serveMonitor(done)

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

// watch is the standing supervision, as opposed to the disconnect handler's
// reaction to one particular event.
//
// The disconnect handler acts on a symptom: the client went away. The hazard
// it stands in for is different and larger - the laser is on and the machine
// is not moving - and that hazard has causes the disconnect handler cannot
// see. LightBurn can hang with its connection intact and its job half sent.
// Our own escalation can fail. A controller can sit in Hold with the output
// still live. So the invariant is watched directly, rather than one of the
// ways it can be broken.
//
// Two conditions, because they carry different risks of being wrong:
//
//   - The laser is on with nobody attached. There is no legitimate version of
//     this, so the grace is short.
//   - The laser is on and the machine has not moved. Piercing thick material
//     is a stationary burn on purpose, and so is aiming the beam by hand, so
//     the grace here has to be longer than any of those - and it is the one
//     number worth putting in the configuration.
//
// It asks the controller for a status report only when nobody else has for a
// while. During a job the client polls several times a second and the
// appliance stays silent; when the conversation stops, it takes over the
// asking. That is the one thing it injects into a client's stream, it is
// read-only, and without it the watchdog would be reading a number that
// stopped updating exactly when it started mattering.
func (b *Bridge) watch(done <-chan struct{}) {
	tick := b.config.IdlePoll / 2
	if tick < 100*time.Millisecond {
		tick = 100 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	silenceReported := false
	var beamOnSince, movedAt time.Time
	var lastPosition grbl.Position

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}
		port := b.currentPort()
		if port == nil {
			beamOnSince, movedAt = time.Time{}, time.Time{}
			continue
		}
		// Only when nobody else is asking.
		if b.observer.Quiet(b.config.IdlePoll) {
			if err := b.writeOwn(port, statusReq); err != nil {
				continue
			}
		}

		if silent := b.controllerSilent(); silent != silenceReported {
			silenceReported = silent
			if silent {
				b.logf("the controller is not answering")
				b.note("fault", "the controller stopped answering")
			}
		}

		machine := b.observer.Machine()
		now := time.Now()
		if machine.HasPosition && (machine.Position != lastPosition || movedAt.IsZero()) {
			lastPosition, movedAt = machine.Position, now
		}
		if machine.Beam != grbl.BeamOn {
			beamOnSince = time.Time{}
			continue
		}
		if beamOnSince.IsZero() {
			beamOnSince = now
		}
		if b.config.OnDisconnect == DisconnectNone {
			continue
		}

		why := ""
		switch {
		case b.currentClient() == nil && now.Sub(beamOnSince) > b.config.BeamGrace:
			why = "the laser was on with nobody connected"
		case b.config.StationaryBeam > 0 && machine.HasPosition &&
			now.Sub(movedAt) > b.config.StationaryBeam && now.Sub(beamOnSince) > b.config.StationaryBeam:
			why = "the laser was on and the machine had not moved for " + b.config.StationaryBeam.String()
		default:
			continue
		}

		beamOnSince, movedAt = time.Time{}, time.Time{}
		b.logf("%s", why)
		b.note("fault", why)
		// TryLock rather than Lock: if the disconnect handler is already
		// working on this, it does not need a second opinion arriving in the
		// middle of its own accounting - the spindle-stop override is a toggle.
		if b.interveneMu.TryLock() {
			b.interveneBeamOn(port, why)
			b.interveneMu.Unlock()
		}
	}
}

// interveneBeamOn stops a laser that should not be on.
//
// The feed hold comes first because the spindle-stop override only acts in the
// HOLD state, and because whatever the machine thinks it is doing, it should
// stop doing it.
func (b *Bridge) interveneBeamOn(port *serial.Port, why string) {
	if b.aborted() {
		return
	}
	if err := b.writeOwn(port, feedHold); err != nil {
		b.setError(err)
		return
	}
	b.noteIntervention("feed hold: " + why)
	b.ensureBeamOff(port, false)
}

// controllerSilent reports whether the controller has stopped answering, or
// never started.
//
// Never having spoken counts, once the port has been open long enough for an
// answer: with the appliance now polling whenever nobody else is, a controller
// that has said nothing at all is a wrong device, a wrong baud rate, or a dead
// board - not simply one nobody has addressed.
func (b *Bridge) controllerSilent() bool {
	b.mu.Lock()
	openedAt := b.portOpenedAt
	b.mu.Unlock()
	if openedAt.IsZero() {
		return false
	}
	if b.observer.Machine().LastReportUnix == 0 {
		return time.Since(openedAt) > b.config.SilenceAfter
	}
	return b.observer.Silent(b.config.SilenceAfter)
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
		case <-done:
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
func (b *Bridge) openPort(done <-chan struct{}) *serial.Port {
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
			case <-done:
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
			// Delayed, because an adapter that was just plugged in may be
			// attached to a controller that is still booting, and a question
			// asked into a reset gets no answer.
			go func() {
				select {
				case <-done:
					return
				case <-time.After(2 * time.Second):
				}
				b.askSettings(port)
			}()
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
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(15 * time.Second)
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

// onClientGone decides what a machine left to itself should do.
//
// A laptop that closes its lid mid-job leaves a controller working through
// whatever is still in its planner buffer, with nobody watching, and on a
// laser that is worse than it sounds: if the stream simply starves, the axes
// stop but the beam is not necessarily switched off with them.
//
// A feed hold stops the motion. Whether it also switches the beam off depends
// on GRBL setting $32: with laser mode on it does, and with laser mode off the
// output is treated as a spindle - which a feed hold deliberately leaves
// running, because a router bit stopping in the cut is its own kind of damage.
// So the hold is where this starts, never where it ends.
//
// None of it is a substitute for a hardware emergency stop. It depends on this
// daemon running, the serial link working and the controller answering.
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
	if machine.State.AtRest() && machine.Beam == grbl.BeamOff {
		// Standing still with the output off. That is a machine on hold - very
		// likely one the watchdog has already dealt with - and holding it
		// again would achieve nothing except an entry in the record implying
		// something was wrong.
		return
	}
	port := b.currentPort()
	if port == nil {
		return
	}
	// One thing commanding the machine at a time; the watchdog may have
	// reached the same conclusion a moment earlier.
	b.interveneMu.Lock()
	defer b.interveneMu.Unlock()

	b.logf("client left while machine was %s; sending feed hold", machine.State)
	if err := b.writeOwn(port, feedHold); err != nil {
		b.setError(err)
		return
	}
	b.noteIntervention("feed hold after the client disconnected while " + string(machine.State))

	// The beam is dealt with before the position. A soft reset while the axes
	// are still turning costs a homing cycle; a beam left burning where it
	// stands costs the workpiece and possibly more, so if the controller says
	// the laser is still on, none of the politeness below applies.
	if b.ensureBeamOff(port, action == DisconnectReset) {
		return
	}
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
	b.softReset(port, "soft reset after the client disconnected mid-job")
}

// ensureBeamOff checks that the hold actually switched the laser off, and does
// something about it if it did not.
//
// The check is not an inference from the state. GRBL fills the accessory field
// of its status report from the actual output, so "A:S" is the controller
// saying the beam is on, and an Ov: field arriving without an accessory beside
// it is the controller saying nothing is on at all.
//
// The escalation is ordered by what it costs. The spindle-stop override stops
// the output and leaves the job resumable; a soft reset guarantees the output
// is off - mc_reset kills the spindle unconditionally - and ends the job.
// It returns true if it soft-reset the controller, so a caller that was about
// to do that itself does not do it twice. resetting says the caller intends a
// reset anyway, which turns "could not confirm" from something worth saying
// into noise.
func (b *Bridge) ensureBeamOff(port *serial.Port, resetting bool) bool {
	switch b.pollBeam(port) {
	case grbl.BeamOff:
		b.logf("controller confirms the beam is off")
		return false

	case grbl.BeamOn:
		// Only ever sent on positive evidence that the beam is on. The
		// override is a TOGGLE: sending it to a controller whose output is
		// already stopped would switch the laser back on.
		b.logf("the beam is still on after the feed hold; stopping the spindle output")
		if err := b.writeOwn(port, spindleStop); err != nil {
			b.setError(err)
			return false
		}
		b.noteIntervention("the beam was still on; stopped the spindle output")
		if b.pollBeam(port) == grbl.BeamOff {
			b.logf("controller confirms the beam is off")
			return false
		}
		b.softReset(port, "the beam stayed on after a feed hold and a spindle stop; soft reset")
		return true

	default:
		// The controller never said. If laser mode is off, that is not an open
		// question - the output is a spindle and a feed hold does not touch a
		// spindle.
		if b.observer.Machine().LaserMode == grbl.SettingOff {
			b.softReset(port, "laser mode ($32) is off, so the feed hold did not switch the beam off; soft reset")
			return true
		}
		// Otherwise: on the balance of what is known, laser mode switches the
		// beam off with the hold. Recorded rather than acted on, because
		// ending a job on an absence of evidence is its own kind of unreliable
		// - and grbl.on_disconnect=reset is there for anyone who would rather.
		if !resetting {
			b.noteIntervention("feed hold sent, but the controller never reported whether the beam is off")
			b.logf("could not confirm the beam is off")
		}
		return false
	}
}

// pollBeam asks the controller about itself until it says something about its
// outputs, or until asking stops being worthwhile.
func (b *Bridge) pollBeam(port *serial.Port) grbl.Beam {
	// Deliberately starting from no answer rather than from whatever was last
	// seen: the question is what the controller says now.
	b.observer.ForgetBeam()

	deadline := time.NewTimer(b.config.HoldSettle)
	defer deadline.Stop()
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := b.writeOwn(port, statusReq); err != nil {
			return grbl.BeamUnknown
		}
		select {
		case <-ticker.C:
			if beam := b.observer.Machine().Beam; beam != grbl.BeamUnknown {
				return beam
			}
			if b.currentClient() != nil || b.aborted() {
				return grbl.BeamUnknown
			}
		case <-deadline.C:
			return b.observer.Machine().Beam
		}
	}
}

func (b *Bridge) softReset(port *serial.Port, why string) {
	if b.aborted() {
		return
	}
	if err := b.writeOwn(port, softReset); err != nil {
		b.setError(err)
		return
	}
	b.noteIntervention(why)
	b.logf("soft reset sent: %s", why)
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
		if err := b.writeOwn(port, statusReq); err != nil {
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

// askSettings requests a settings dump, which is how the appliance learns
// whether laser mode is on before it matters rather than afterwards.
//
// Read-only, like the status request, and sent only with nobody attached so a
// client never sees an answer to a question it did not ask.
func (b *Bridge) askSettings(port *serial.Port) {
	if b.currentClient() != nil || b.aborted() {
		return
	}
	if _, err := port.Write([]byte("$$\n")); err != nil {
		return
	}
	b.monitors.send('*', []byte("$$\n"))
}

// writeOwn sends a command the bridge decided to send, as opposed to one a
// client asked for. Anyone watching the monitor port sees it marked as ours: a
// transcript that showed the appliance's own feed hold as if the client had
// sent it would mislead about the one thing worth watching for.
func (b *Bridge) writeOwn(port *serial.Port, command byte) error {
	if _, err := port.Write([]byte{command}); err != nil {
		return err
	}
	b.monitors.send('*', []byte{command})
	return nil
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
	machine := b.observer.Machine()
	event := Event{Kind: "intervention", Text: what, State: machine.State, Context: b.sent.Lines()}
	if machine.HasPosition {
		position := machine.Position
		event.Position = &position
	}
	b.journal.Add(event)
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
