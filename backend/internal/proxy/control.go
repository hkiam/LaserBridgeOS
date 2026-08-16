package proxy

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/serial"
)

// What an operator may ask the appliance to do, and the rule that makes it
// safe to offer at all.
//
// Everything else in this package is the bridge acting on its own judgement,
// narrowly and with a written justification. This is the opposite: the machine
// doing what somebody at the web interface asked. That is a different kind of
// risk and it gets a different kind of guard.
//
// The rule: while a client is connected, the appliance commands nothing. Two
// applications steering one laser is the failure this whole appliance exists to
// prevent, and a jog sent into LightBurn's stream would also hand it an "ok" it
// never earned - the same corruption ADR 0013 forbids for $$ - after which the
// G-code the machine runs is not the G-code that was sent.
//
// There is exactly one exception, and it is the one that ends the conflict
// rather than joining it: Stop. Whoever presses it means the machine to be
// stopped, and leaving a sender streaming into a controller that has just been
// reset is the hole ADR 0018 closed. So Stop takes the machine over the same
// way the watchdog does - the client is told and let go - and everything else
// is refused while somebody else is steering.
type Command string

const (
	CommandHome      Command = "home"
	CommandUnlock    Command = "unlock"
	CommandHold      Command = "hold"
	CommandResume    Command = "resume"
	CommandStop      Command = "stop"
	CommandJog       Command = "jog"
	CommandJogCancel Command = "jog-cancel"
	CommandAim       Command = "aim"
	CommandAimOff    Command = "aim-off"
	// CommandGoTo moves to a position instead of by a distance, and
	// CommandSend carries a line the operator typed. Both are the jog pad's
	// argument taken one step further: an operator who can move a machine one
	// millimetre at a time can already put it anywhere, and refusing to let
	// them say where they want it only makes them press the arrow forty times.
	CommandGoTo Command = "goto"
	CommandSend Command = "send"
	// CommandZero sets the work origin here. It is the other half of showing
	// work coordinates at all: a reading in a coordinate system nobody can set
	// is a reading of the wrong thing, and setting it by hand means typing
	// G10 L20 P1 X0 from memory.
	CommandZero Command = "zero"
)

// CommandRequest is an operator's command on its way to the daemon, and
// CommandAnswer is what came back. An empty Error means it was sent; it does
// not mean the machine did anything, which only the machine can say.
//
// It travels over the status socket as JSON and is also what Do takes, so
// there is one description of a command rather than one per hop.
type CommandRequest struct {
	Command Command `json:"command"`
	// Axis, Distance and Feed describe a jog: a relative move.
	Axis     string  `json:"axis,omitempty"`
	Distance float64 `json:"distance,omitempty"`
	Feed     float64 `json:"feed,omitempty"`
	// X, Y and Z describe a go-to: an absolute position in work coordinates.
	// An axis that is not given is left where it is, which is what makes
	// "go to X0 Y0" mean the work origin and not "drop the head to Z0".
	X *float64 `json:"x,omitempty"`
	Y *float64 `json:"y,omitempty"`
	Z *float64 `json:"z,omitempty"`
	// Percent is the aiming beam's power.
	Percent int `json:"percent,omitempty"`
	// Line is a command typed by an operator, sent as it stands.
	Line string `json:"line,omitempty"`
	// Holder identifies who is asking - a browser session, not a person. It
	// decides who may hold the aiming beam and it goes into the record.
	Holder string `json:"holder,omitempty"`
}

type CommandAnswer struct {
	Error string `json:"error,omitempty"`
}

var (
	// ErrClientInCharge is returned for everything but Stop while a client is
	// connected.
	ErrClientInCharge = errors.New("a client is connected; the appliance does not steer a machine somebody else is steering")
	// ErrNoPort is returned when there is no serial port to command.
	ErrNoPort = errors.New("no serial port is open")
	// ErrUnknownCommand is a caller asking for something that does not exist.
	ErrUnknownCommand = errors.New("unknown command")
)

// Jog is a single relative move.
type Jog struct {
	// Axis is X, Y or Z; Distance is in millimetres and may be negative.
	Axis     string
	Distance float64
	Feed     float64
}

// aimLease is how long an aiming beam may stay on without being asked for
// again.
//
// The aiming beam is the appliance's own hazard, and the reason it needs a
// lease rather than a switch: with nobody connected, a beam that is on is
// exactly what TriggerBeamUnattended exists to stop, and it would stop it
// within a second. Rather than carve an exception out of the watchdog, the
// operator holds a lease that says "I am here" - and the page renews it every
// second. A closed laptop, a lost network, a browser tab that crashed: the
// lease runs out, the beam goes off, and the watchdog is watching again.
const aimLease = 3 * time.Second

// aimingBeam holds the lease, and who holds it.
//
// The holder matters. Without one, anybody on the workshop network can renew
// somebody else's beam - or take it over mid-alignment - and neither of them
// would know the other was there. That is the same failure as two applications
// steering one laser, one floor up: the appliance refuses that one and would
// have permitted this. The holder is derived from the browser's session, so a
// second window is a second holder even on the same machine.
type aimingBeam struct {
	mu     sync.Mutex
	until  time.Time
	holder string
	// percent is what was last asked for, so the record can say.
	percent int
}

// ErrSomebodyElseIsAiming is a second holder asking for a beam that is already
// being held.
var ErrSomebodyElseIsAiming = errors.New("somebody else is holding the aiming beam")

// hold takes or renews the lease. A different holder is refused while the
// current one is still live.
func (a *aimingBeam) hold(holder string, percent int, now time.Time) (first bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	live := !a.until.IsZero() && now.Before(a.until)
	if live && a.holder != holder {
		return false, ErrSomebodyElseIsAiming
	}
	a.until, a.holder, a.percent = now.Add(aimLease), holder, percent
	return !live, nil
}

// releaseBy lets go, but only for whoever is holding it. An empty holder is
// the appliance itself - a soft reset, a shutdown - and always allowed.
func (a *aimingBeam) releaseBy(holder string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if holder != "" && a.holder != "" && a.holder != holder && !a.until.IsZero() {
		return ErrSomebodyElseIsAiming
	}
	a.until, a.holder = time.Time{}, ""
	return nil
}

func (a *aimingBeam) release() {
	_ = a.releaseBy("")
}

// live reports whether somebody is currently holding the beam on.
func (a *aimingBeam) live(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.until.IsZero() && now.Before(a.until)
}

// expired reports a lease that has run out and clears it, so the caller can
// switch the beam off exactly once.
func (a *aimingBeam) expired(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.until.IsZero() || now.Before(a.until) {
		return false
	}
	a.until = time.Time{}
	return true
}

// Aiming reports whether an aiming beam is being held right now.
func (b *Bridge) Aiming() bool { return b.aim.live(time.Now()) }

// Do carries out an operator's command.
//
// holder identifies who is asking, derived from their browser session. It
// decides who may hold the aiming beam and it goes into the record, because a
// journal that says the machine moved without saying who moved it answers the
// wrong question.
func (b *Bridge) Do(request CommandRequest) error {
	command, holder := request.Command, request.Holder
	port := b.currentPort()
	if port == nil {
		return ErrNoPort
	}
	if command == CommandStop {
		// The one command allowed while somebody else is steering, and it ends
		// their session on the way past - see the note at the top of this file.
		b.interveneMu.Lock()
		defer b.interveneMu.Unlock()
		b.takeOverFromClient("stopped from the web interface")
		b.aim.release()
		if err := b.writeOwn(port, softReset); err != nil {
			return err
		}
		b.noteIntervention("soft reset, asked for from the web interface")
		return nil
	}
	if b.currentClient() != nil {
		return ErrClientInCharge
	}

	switch command {
	case CommandHold:
		return b.commanded(command, holder, "feed hold", b.writeOwn(port, feedHold))
	case CommandResume:
		return b.commanded(command, holder, "resume", b.writeOwn(port, resume))
	case CommandJogCancel:
		return b.commanded(command, holder, "jog cancelled", b.writeOwn(port, jogCancel))
	case CommandHome:
		return b.commanded(command, holder, "$H", b.sendLine(port, "$H"))
	case CommandUnlock:
		return b.commanded(command, holder, "$X", b.sendLine(port, "$X"))
	case CommandJog:
		line, err := jogLine(Jog{Axis: request.Axis, Distance: request.Distance, Feed: request.Feed})
		if err != nil {
			return err
		}
		return b.commanded(command, holder, line, b.sendLine(port, line))
	case CommandGoTo:
		line, err := gotoLine(request)
		if err != nil {
			return err
		}
		return b.commanded(command, holder, line, b.sendLine(port, line))
	case CommandZero:
		line, err := zeroLine(request.Axis)
		if err != nil {
			return err
		}
		return b.commanded(command, holder, line, b.sendLine(port, line))
	case CommandSend:
		line, err := typedLine(request.Line)
		if err != nil {
			return err
		}
		return b.commanded(command, holder, line+" (typed)", b.sendLine(port, line))
	case CommandAim:
		percent := request.Percent
		if percent < 1 || percent > 100 {
			return errors.New("aiming power must be between 1 and 100 percent")
		}
		first, err := b.aim.hold(holder, percent, time.Now())
		if err != nil {
			return err
		}
		if !first {
			// A renewal. The beam is already on and saying so again would put a
			// line in the controller's buffer every second for nothing - and
			// would fill the record with one line a second.
			return nil
		}
		line := "M3 S" + strconv.Itoa(b.spindleFor(percent))
		return b.commanded(command, holder, line+" (aiming beam at "+strconv.Itoa(percent)+"%)", b.sendLine(port, line))
	case CommandAimOff:
		if err := b.aim.releaseBy(holder); err != nil {
			return err
		}
		return b.commanded(command, holder, "M5", b.sendLine(port, "M5"))
	default:
		return fmt.Errorf("%w: %q", ErrUnknownCommand, command)
	}
}

// commanded records what an operator asked for, once it has actually been sent.
//
// The record was built to answer "why did the machine stop?" and answered it
// well. Now that the appliance can be asked to move a machine there is a second
// question - "who moved it?" - and until this, a jog, a homing cycle or a feed
// hold from the web interface left no trace at all. The first time something
// goes wrong during manual work, a record that is silent about the manual work
// is silent about the only thing that mattered.
func (b *Bridge) commanded(command Command, holder, what string, err error) error {
	if err != nil {
		return err
	}
	who := holder
	if who == "" {
		who = "the appliance"
	}
	b.note("command", string(command)+" from "+who+": "+what)
	return nil
}

// spindleFor turns a percentage into the S value this controller understands.
// $30 is the maximum, learned from the settings dump; GRBL's own default is
// 1000 and that is what is assumed when the controller never said.
func (b *Bridge) spindleFor(percent int) int {
	maximum := b.observer.Machine().SpindleMax
	if maximum <= 0 {
		maximum = 1000
	}
	value := maximum * percent / 100
	if value < 1 {
		value = 1
	}
	return value
}

func jogLine(jog Jog) (string, error) {
	axis := strings.ToUpper(strings.TrimSpace(jog.Axis))
	if axis != "X" && axis != "Y" && axis != "Z" {
		return "", errors.New("jog axis must be X, Y or Z")
	}
	if jog.Distance == 0 || jog.Distance < -1000 || jog.Distance > 1000 {
		return "", errors.New("jog distance must be between -1000 and 1000 millimetres, and not zero")
	}
	if jog.Feed < 1 || jog.Feed > 20000 {
		return "", errors.New("jog feed must be between 1 and 20000")
	}
	// G91 relative, G21 millimetres, stated every time: a jog that inherited a
	// G20 from whatever ran before it would move twenty-five times too far.
	return fmt.Sprintf("$J=G91 G21 %s%.3f F%.0f", axis, jog.Distance, jog.Feed), nil
}

// gotoLine turns a target into a jog rather than into a G0.
//
// A jog is the safer of the two and by some distance. It never changes the
// modal state, so it cannot leave the machine in G91 for whatever runs next; it
// is answered when it is accepted rather than when it arrives, so the pad stays
// responsive; and it is cancelled by the same Cancel button as any other jog,
// which a G0 would ignore. The coordinates are in the work coordinate system,
// which is the one the reading in front of the operator shows.
func gotoLine(request CommandRequest) (string, error) {
	line := "$J=G90 G21"
	for _, axis := range []struct {
		name  string
		value *float64
	}{{"X", request.X}, {"Y", request.Y}, {"Z", request.Z}} {
		if axis.value == nil {
			continue
		}
		if *axis.value < -10000 || *axis.value > 10000 {
			return "", errors.New("a target coordinate must be between -10000 and 10000 millimetres")
		}
		line += fmt.Sprintf(" %s%.3f", axis.name, *axis.value)
	}
	if line == "$J=G90 G21" {
		return "", errors.New("a go-to needs at least one coordinate")
	}
	if request.Feed < 1 || request.Feed > 20000 {
		return "", errors.New("feed must be between 1 and 20000")
	}
	return line + fmt.Sprintf(" F%.0f", request.Feed), nil
}

// zeroLine sets the work origin for one axis or for all of them.
//
// G10 L20 P1 sets the offset of coordinate system 1 - G54, the one a machine
// powers up in - so that the current position reads as the value given. It is
// written to the controller's EEPROM and survives a power cycle, which is what
// makes it worth a button and worth being careful about: this is the only
// command the steering sends that outlives the session.
func zeroLine(axis string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(axis)) {
	case "X":
		return "G10 L20 P1 X0", nil
	case "Y":
		return "G10 L20 P1 Y0", nil
	case "Z":
		return "G10 L20 P1 Z0", nil
	case "", "ALL":
		return "G10 L20 P1 X0 Y0 Z0", nil
	default:
		return "", errors.New("an axis to zero must be X, Y, Z or all")
	}
}

// typedLine checks a line an operator wrote by hand.
//
// This is the one place where the appliance sends something it has not composed
// itself, and the checks are about what can be expressed rather than about what
// it means. Deciding whether "G1 X500" is sensible needs to know the machine,
// the material and the intent, and the appliance knows none of the three - the
// operator does. What it can insist on is one line, printable, and short enough
// that it cannot overrun the controller's buffer on its own.
//
// The real-time bytes are refused on purpose. They are not lines, they jump the
// queue, and every one of them already has a button that accounts for it
// properly - a soft reset typed in here would leave the appliance's own line
// bookkeeping believing in commands the controller had already forgotten.
const maxTypedLine = 80

func typedLine(line string) (string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", errors.New("nothing to send")
	}
	if len(line) > maxTypedLine {
		return "", fmt.Errorf("a typed command may be at most %d characters", maxTypedLine)
	}
	for _, c := range []byte(line) {
		if c < 0x20 || c > 0x7e {
			return "", errors.New("a typed command may only contain printable ASCII, and only one line at a time")
		}
	}
	// The soft reset is 0x18 and has already been refused as unprintable; the
	// other three are printable and have to be named.
	switch line {
	case "?", "!", "~":
		return "", errors.New("the real-time keys have their own buttons: the status is read continuously, and hold, resume and stop are beside the pad")
	}
	return line, nil
}

// outstandingLines is how many injected lines may be waiting for their "ok".
//
// GRBL's receive buffer is 128 bytes and the longest line sent from here is
// about thirty, so three in flight is comfortably inside it with room for the
// controller's own replies. This is the counting the project insists on for a
// client's stream, applied to our own: without it, somebody tapping the jog pad
// faster than the machine answers pushes characters into a full buffer, and
// what the controller then parses is not what was sent.
const outstandingLines = 3

// lineAnswerTimeout releases a slot for a line that was never answered. Homing
// can take a minute on a large machine and its "ok" comes at the end, so this
// has to be generous; it exists so that one lost reply does not wedge the pad
// for good.
const lineAnswerTimeout = 90 * time.Second

// ErrStillWorking is a caller sending faster than the controller answers.
var ErrStillWorking = errors.New("the controller has not answered the last commands yet")

// acknowledged is called for every "ok" the controller sends, which releases
// the oldest line waiting for one.
func (b *Bridge) acknowledged() {
	select {
	case <-b.injected:
	default:
	}
}

// sendLine writes a queued command, which is only ever done with no client
// attached - Do enforces that, and this is the second pair of eyes on it.
//
// GRBL answers a queued command with "ok", and a sender counts those against
// its own idea of the 128-byte receive buffer. One it never earned and the
// stream is corrupt from that point on, which is why this exists as its own
// function with its own warning rather than as a call to port.Write - and why
// it counts its own lines out and back.
//
// It does not wait for the machine to finish moving. A jog is answered when it
// is accepted into the planner, and queueing the next one while the last is
// still running is what makes jogging continuous rather than a series of
// twitches. What it waits for is the answer, not the motion.
func (b *Bridge) sendLine(port *serial.Port, line string) error {
	if b.currentClient() != nil {
		return ErrClientInCharge
	}
	select {
	case b.injected <- time.Now():
	default:
		return ErrStillWorking
	}
	if _, err := port.Write([]byte(line + "\n")); err != nil {
		b.acknowledged()
		return err
	}
	b.monitors.send('*', []byte(line+"\n"))
	return nil
}

// forgetStaleLines releases slots held by lines the controller never answered.
// Called from the watchdog, which is the loop that runs whether or not anybody
// is asking for anything.
func (b *Bridge) forgetStaleLines(now time.Time) {
	for {
		select {
		case sent := <-b.injected:
			if now.Sub(sent) < lineAnswerTimeout {
				// Still young: put it back and stop looking, because the
				// channel is in order and everything behind it is younger.
				select {
				case b.injected <- sent:
				default:
				}
				return
			}
			b.logf("a command sent %s ago was never answered; releasing its slot", now.Sub(sent).Round(time.Second))
		default:
			return
		}
	}
}
