package proxy

import (
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
func (b *Bridge) onClientGone() {
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
