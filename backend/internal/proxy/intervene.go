package proxy

import (
	"net"
	"strings"
	"time"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/grbl"
	"github.com/laserbridgeos/laserbridgeos/backend/internal/serial"
)

// onClientGone reacts to the client having disappeared.
//
// A laptop that closes its lid mid-job leaves a controller working through
// whatever is still in its planner buffer with nobody watching, and on a laser
// that is worse than it sounds: if the stream starves, the axes stop but the
// beam is not necessarily switched off with them. What to do about that is
// Decide's business; this decides only whether the question arises.
func (b *Bridge) onClientGone(conn net.Conn) {
	// Counted whatever the outcome, including "nothing to do": it is what lets
	// a test wait for the decision to have been made rather than sleep and
	// hope.
	defer b.handled.Add(1)

	// A daemon being stopped is not a client walking away. Somebody asked for
	// this - a service restart after a settings change, say - so the machine
	// is not unattended and the job should not be interrupted.
	if b.aborted() {
		return
	}
	// A client the bridge dropped itself, in the middle of intervening, has
	// already had the machine dealt with. Without this the disconnect it just
	// caused would start a second ladder and send a soft reset to a controller
	// that was reset a moment ago.
	b.clientMu.Lock()
	ours := conn != nil && b.droppedByUs == conn
	if ours {
		b.droppedByUs = nil
	}
	b.clientMu.Unlock()
	if ours {
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
	b.intervene(port, TriggerClientGone, "the client disconnected")
}

// intervene carries out whatever Decide asks for, one step at a time, looking
// again after each.
//
// Looking again is the point. Every rung either works or does not, and the
// controller is the only authority on which: GRBL fills the accessory field of
// its status report from the actual output. The loop is bounded because each
// step sets a flag that stops it being recommended twice, and a table test
// walks every reachable state to prove no situation loops.
func (b *Bridge) intervene(port *serial.Port, trigger Trigger, why string) {
	tried := Tried{}
	takenOver := false
	for step := 0; step < 5; step++ {
		if b.aborted() {
			return
		}
		machine := b.observer.Machine()
		decision := Decide(Situation{
			Policy:    b.config.OnDisconnect,
			Trigger:   trigger,
			State:     machine.State,
			Beam:      machine.Beam,
			LaserMode: machine.LaserMode,
			Tried:     tried,
		})

		// Every entry says what was done and what prompted it. An operator
		// reading the record afterwards has neither in front of them.
		record := func() {
			b.noteIntervention(decision.Reason + " (" + why + ")")
			b.logf("%s (%s)", decision.Reason, why)
		}

		// The three steps below command the machine, and a client that is still
		// streaming has to be let go before the first of them. See takeOver.
		// Not for TriggerClientGone: there is no client to take it from, and a
		// newcomer arriving mid-intervention is handed the machine rather than
		// dropped - the loop below already returns for that case.
		switch decision.Step {
		case StepFeedHold, StepSpindleStop, StepSoftReset:
			if !takenOver && trigger != TriggerClientGone {
				b.takeOverFromClient(why)
				takenOver = true
			}
		}

		switch decision.Step {
		case StepNone:
			return

		case StepUnconfirmed:
			record()
			return

		case StepFeedHold:
			if err := b.writeOwn(port, feedHold); err != nil {
				b.setError(err)
				return
			}
			tried.FeedHold = true
			record()

		case StepSpindleStop:
			// Only ever on positive evidence that the beam is on, and only in
			// HOLD - Decide guarantees both. The override is a TOGGLE, so
			// sending it to a controller whose output is already stopped would
			// switch the laser back on.
			if err := b.writeOwn(port, spindleStop); err != nil {
				b.setError(err)
				return
			}
			tried.SpindleStop = true
			record()

		case StepSoftReset:
			if decision.WaitForRest {
				// A soft reset while the axes are still turning loses the
				// position. Worth waiting for only when the beam is not the
				// thing being put out.
				if !b.waitUntilStill(port) {
					b.logf("machine did not come to rest within %s; resetting anyway", b.config.HoldSettle)
				}
				if b.aborted() {
					return
				}
			}
			if err := b.writeOwn(port, softReset); err != nil {
				b.setError(err)
				return
			}
			tried.SoftReset = true
			record()
		}

		// Ask the controller what that achieved before deciding again.
		b.pollBeam(port)
		// A newcomer taking charge ends a reaction to their predecessor
		// leaving, but not a stationary beam - that is not about who is
		// connected.
		if b.currentClient() != nil && trigger == TriggerClientGone {
			return
		}
	}
}

// takeOverFromClient ends the connection of a client that is still streaming,
// having first told it why.
//
// Silence here was a hole, and it was found on a real machine: the watchdog
// stopped a laser, and LightBurn - whose connection was never in question -
// carried on sending. That is worse than not intervening at all. A soft reset
// empties GRBL's planner and its 128-byte receive buffer and, after motion,
// leaves the controller in alarm; a sender that knows none of this keeps
// counting "ok"s against a buffer that no longer holds what it thinks. What
// follows is either a stream of error:9, or - if the controller came back idle
// - G-code executed while the two ends disagree about where the job is. This
// project's own words for that are "not a failed job, it is a wrong cut".
//
// ADR 0013 justified injecting real-time commands for the case where the client
// had already gone. The stationary-beam watchdog then reused the mechanism for
// the case where it has not, which is precisely the case the justification did
// not cover.
//
// So the rule is: whoever takes the machine takes it completely. The [MSG:...]
// is GRBL's own way of speaking to a sender and appears in LightBurn's console;
// it cannot disturb the "ok" accounting because it is not an "ok". The
// disconnect is what actually stops the streaming, and the message is what
// explains it afterwards.
func (b *Bridge) takeOverFromClient(why string) {
	b.clientMu.Lock()
	client := b.client
	// Noted before the close, because closing is what wakes the goroutine that
	// would otherwise start a second intervention.
	b.droppedByUs = client
	b.clientMu.Unlock()
	if client == nil {
		return
	}
	// A deadline of its own, and a short one. The ordinary write timeout is
	// five seconds, which is the right patience for a client that is receiving
	// a job - and exactly the wrong patience here: the case this whole
	// mechanism exists for is a sender that has stopped reading, so its receive
	// window is full and this write is the one that blocks. Five seconds of
	// waiting to be polite, before the laser is dealt with, would make the
	// courtesy cost more than it is worth. The message is a courtesy; the stop
	// is not.
	_ = client.SetWriteDeadline(time.Now().Add(dropMessageTimeout))
	_, _ = client.Write([]byte("[MSG:LaserBridgeOS took over: " + oneLine(why) + "]\r\n"))
	if note := b.jobs.end("stopped by the bridge", time.Now()); note != "" {
		b.note("job", note)
	}
	b.logf("dropping client %s: %s", client.RemoteAddr(), why)
	// Recorded as an intervention, not as a client event: routine connects and
	// disconnects stay in memory, and this one has to survive the restart that
	// so often follows it. It is also literally something the bridge did on its
	// own accord, which is what that kind means.
	b.note("intervention", "dropped "+client.RemoteAddr().String()+" because the bridge took the machine over: "+why)
	_ = client.Close()
}

// dropMessageTimeout bounds how long the bridge will wait to tell a client
// why it is being dropped. Long enough for a client that is reading its socket,
// short enough that one that is not costs a fraction of a second.
const dropMessageTimeout = 200 * time.Millisecond

// oneLine keeps a reason inside a GRBL message. Square brackets end the
// message and a line ending ends the line, so neither may travel inside one.
func oneLine(text string) string {
	return strings.NewReplacer("[", "(", "]", ")", "\r", " ", "\n", " ").Replace(text)
}

// The opening questions, and the windows they have to fit in.
//
// The numbers come from what the first appliance to run this did, and the first
// version of them was wrong in an instructive way. Opening the serial port
// toggles DTR, which resets an Arduino-based controller - that is why the
// bridge holds the port open for its whole life (ADR 0009) - and a controller
// that has just been reset says nothing at all for a second or two before it
// announces itself. The probe waited 1.5 seconds for a sign of life; the banner
// arrived at 2. It gave up on a reset it had caused itself, every single boot.
//
// probeIdentify is now long enough for that reset and the boot after it,
// probeSettings for the settings dump that follows, and probeGrace - how long a
// connecting client waits - is longer than both together, because the gate only
// means something while it is closed. The grace is derived rather than fixed,
// so that shortening the budget in a test shortens the wait with it.
const (
	probeIdentify = 5 * time.Second
	probeSettings = 2 * time.Second
)

func (b *Bridge) probeGrace() time.Duration {
	return b.config.ProbeIdentify + probeSettings + time.Second
}

// probeController asks the controller what it is, in the one window where that
// question is safe to ask.
//
// ADR 0013 forbids the bridge from injecting anything that GRBL answers with an
// "ok", and $$ and $I are exactly that. Senders count those "ok"s to know how
// much of the 128-byte receive buffer is free; one that the sender never earned
// makes it believe a line was accepted that was not, and it then writes past the
// end of the buffer. Characters are dropped and the G-code the machine runs is
// not the G-code that was sent.
//
// The ADR also rejected the obvious guard, "only when no client is attached",
// and it was right to: the answer comes back fifty milliseconds later, by which
// time a client may have connected and be handed an "ok" it did not earn.
//
// What makes it safe is not a check but the order of events. The questions are
// asked in the moment after the serial port opens, and the accept loop does not
// hand any connection to serveClient until they are done. A client can be
// accepted at the TCP level during that window, but b.client stays nil, so the
// dump is forwarded to nobody and the connection starts on a clean slate with
// its line accounting untouched.
//
// The prize is $32. Everything the appliance does about an unattended laser
// turns on whether laser mode is on, and until now it was learned only if a
// client happened to ask - LightBurn sends $I and $#, and on the machine this
// was written for it never sent $$, so the setting stayed unknown and the best
// informed branch of the ladder never ran.
func (b *Bridge) probeController(port *serial.Port) {
	defer b.finishProbe()
	if b.currentClient() != nil || b.aborted() {
		return
	}
	// Nothing is asked of a device that has not already answered like GRBL.
	// $$ is meaningless to a Ruida or a Trocen board and there is no telling
	// what it means to one nobody has tried, whereas a well-formed status
	// report is proof of what is at the other end. It costs no extra bytes to
	// wait for one: the watchdog polls whenever nobody else is, so the question
	// has already been asked by the time this is looking.
	if !b.answersLikeGRBL() {
		// Said out loud rather than passed over. A record that contains nothing
		// about the controller cannot be told apart from one where nobody
		// looked, and this is the line that says which - it was how the failure
		// above was found in the first place.
		b.noteController()
		return
	}
	for _, question := range []string{"$I\n", "$$\n"} {
		if b.currentClient() != nil || b.aborted() {
			return
		}
		if _, err := port.Write([]byte(question)); err != nil {
			return
		}
		b.monitors.send('*', []byte(question))
	}

	deadline := time.NewTimer(probeSettings)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			b.noteController()
			return
		case <-ticker.C:
			if b.currentClient() != nil || b.aborted() {
				return
			}
			if b.observer.Machine().LaserMode != grbl.SettingUnknown {
				b.noteController()
				return
			}
		}
	}
}

// answersLikeGRBL waits for the controller to give itself away, whichever way
// it does so first.
//
// Two signals count. A status report that parsed is one. The welcome banner is
// the other, and on the machine this was written for it is the one that
// arrives: opening the port resets the controller, and what comes back two
// seconds later is "Grbl 1.1h ['$' for help]". Waiting only for a status report
// meant waiting for an answer to a question asked of a board that was still
// booting.
func (b *Bridge) answersLikeGRBL() bool {
	deadline := time.NewTimer(b.config.ProbeIdentify)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			return false
		case <-ticker.C:
			if b.currentClient() != nil || b.aborted() {
				return false
			}
			if b.observer.Gibberish() {
				return false
			}
			machine := b.observer.Machine()
			if machine.Reports > 0 || machine.Resets > 0 {
				return true
			}
		}
	}
}

// noteController writes down what the controller turned out to be. The record
// then says which firmware every later entry was talking to.
func (b *Bridge) noteController() {
	machine := b.observer.Machine()
	what := "controller did not identify itself"
	if machine.Firmware != "" {
		what = "controller: " + machine.Firmware
	}
	if machine.Options != "" {
		what += " (" + machine.Options + ")"
	}
	switch machine.LaserMode {
	case grbl.SettingOn:
		what += "; laser mode ($32) is on"
	case grbl.SettingOff:
		what += "; laser mode ($32) is OFF - a feed hold will not switch the output off"
	default:
		what += "; laser mode ($32) unknown"
	}
	b.logf("%s", what)
	b.note("device", what)
}

// beginProbe closes the accept gate again for another round of questions. A
// client that arrives meanwhile waits at the TCP level, exactly as it does
// while the first questions are being asked.
func (b *Bridge) beginProbe() {
	b.probeMu.Lock()
	defer b.probeMu.Unlock()
	if !b.probeClosed {
		// A probe is already running, and its gate is the one to wait on.
		return
	}
	b.probed = make(chan struct{})
	b.probeClosed = false
}

// probeGate is what a waiting connection watches.
func (b *Bridge) probeGate() <-chan struct{} {
	b.probeMu.Lock()
	defer b.probeMu.Unlock()
	return b.probed
}

func (b *Bridge) finishProbe() {
	b.probeMu.Lock()
	defer b.probeMu.Unlock()
	if b.probeClosed {
		return
	}
	close(b.probed)
	b.probeClosed = true
}

// beamPollInterval is how fast the bridge asks while it is waiting for the
// controller to mention its outputs. Only ever during an intervention, when the
// client has been let go, so it competes with nobody.
const beamPollInterval = 100 * time.Millisecond

// pollBeam asks the controller about itself until it says something about its
// outputs, or until asking stops being worthwhile.
//
// How long that takes is set by the controller, not by us: GRBL mentions its
// outputs in the Ov: block, which it prints every tenth status report when idle
// and every twentieth while moving, plus immediately whenever the accessory
// state changes. The last part is what usually makes this quick - a feed hold
// that switched the beam off is a change, and the change forces the block out
// at once. It is the case where nothing changed, which is the case worth
// knowing about, that has to wait for the counter.
func (b *Bridge) pollBeam(port *serial.Port) grbl.Beam {
	// Deliberately starting from no answer rather than from whatever was last
	// seen: the question is what the controller says now.
	b.observer.ForgetBeam()

	deadline := time.NewTimer(b.config.BeamConfirm)
	defer deadline.Stop()
	ticker := time.NewTicker(beamPollInterval)
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

// writeOwn sends a command the bridge decided to send, as opposed to one a
// client asked for. Anyone watching the monitor port sees it marked as ours: a
// transcript that showed the appliance's own feed hold as if the client had
// sent it would mislead about the one thing worth watching for.
//
// Only ever a real-time byte, and that is not a stylistic preference. GRBL
// answers a real-time command with a status report or with nothing, and a
// queued command - anything ending in a newline, including "$$" - with an
// "ok". Senders count those "ok"s to know how much of GRBL's 128-byte buffer
// is free. One extra, from a line the sender never wrote, and it believes a
// line was accepted that was not: it sends past the end of the buffer,
// characters are dropped, and the G-code the machine executes is not the
// G-code that was sent. An injected question must never be answerable with
// "ok".
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
	ctx := b.ctx
	b.mu.Unlock()
	return ctx != nil && ctx.Err() != nil
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
