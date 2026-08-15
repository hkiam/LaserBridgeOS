package proxy

import "github.com/laserbridgeos/laserbridgeos/backend/internal/grbl"

// What the bridge does about a machine nobody is driving, expressed as a
// decision separated from the act of carrying it out.
//
// The separation earns its keep. Every rung of this ladder depends on a fact
// about GRBL that this project cannot test - that a feed hold is ignored from
// Idle, that the spindle-stop override acts only in HOLD, that a soft reset
// switches the output off unconditionally - and the previous version wove
// those facts into a function that also opened sockets and slept. Every case
// needed a pseudo-terminal, a simulated controller and a second of wall clock,
// which meant the cases nobody thought of stayed untested. One of them was
// wrong: a machine standing still with the laser on got two commands it
// ignores before anything happened. It took a corrected simulator to notice.
//
// As a pure function the whole ladder is a table, checked in microseconds,
// and a case that was not considered is a visibly missing row.

// Trigger is why an intervention is being considered.
type Trigger string

const (
	// TriggerClientGone is the client having disconnected. It is the only
	// trigger that carries the configured policy through to the end: with
	// on_disconnect=reset the machine is reset whether or not the beam was
	// ever on, because the job is over either way.
	TriggerClientGone Trigger = "client-gone"
	// TriggerBeamUnattended is the laser being on with nobody connected.
	TriggerBeamUnattended Trigger = "beam-unattended"
	// TriggerBeamStationary is the laser being on with the machine not moving.
	TriggerBeamStationary Trigger = "beam-stationary"
)

// Step is one thing to send, or one thing to conclude.
type Step int

const (
	// StepNone: nothing needs doing.
	StepNone Step = iota
	StepFeedHold
	StepSpindleStop
	StepSoftReset
	// StepUnconfirmed: the beam could not be shown to be off, and there is no
	// evidence that it is on. Recorded rather than acted on.
	StepUnconfirmed
)

// Tried records what has already been sent during this intervention, so the
// decision cannot recommend the same rung twice.
type Tried struct {
	FeedHold    bool
	SpindleStop bool
	SoftReset   bool
}

// Situation is everything a decision depends on.
type Situation struct {
	Policy  string
	Trigger Trigger
	// State, Beam and LaserMode are the controller's own account of itself.
	State     grbl.MachineState
	Beam      grbl.Beam
	LaserMode grbl.Setting
	Tried     Tried
}

// Decision is the next step and why.
type Decision struct {
	Step Step
	// WaitForRest applies to a soft reset: one sent while the axes are still
	// turning loses the position and lands the controller in an alarm. It is
	// false when the beam is on, because a beam burning where it stands
	// outranks a homing cycle.
	WaitForRest bool
	// Reason is what goes in the record, in words an operator can act on.
	Reason string
}

// Decide returns the next step for a situation, or StepNone when there is
// nothing left to do.
func Decide(s Situation) Decision {
	// none is a promise that the bridge never commands the machine.
	if s.Policy == DisconnectNone {
		return Decision{Step: StepNone}
	}

	// Nothing to stop: not moving, and the laser is not known to be on.
	notMoving := !s.State.Moving()
	if notMoving && s.Beam != grbl.BeamOn && s.Tried == (Tried{}) {
		return Decision{Step: StepNone}
	}
	if s.State.AtRest() && s.Beam == grbl.BeamOff && s.Tried == (Tried{}) {
		return Decision{Step: StepNone}
	}

	// Stop the motion first, if there is any. GRBL enters HOLD only from a
	// cycle or a jog, so sending this to a machine that is already standing
	// still would claim something happened that did not.
	if !s.Tried.FeedHold && !s.State.AtRest() {
		return Decision{Step: StepFeedHold, Reason: "feed hold"}
	}

	switch s.Beam {
	case grbl.BeamOn:
		// The override acts only in HOLD. Anywhere else it is ignored, and
		// trying it anyway costs another second of beam for nothing.
		if s.State == grbl.StateHold && !s.Tried.SpindleStop {
			return Decision{Step: StepSpindleStop, Reason: "the beam is still on; stopping the spindle output"}
		}
		if s.Tried.SoftReset {
			// Everything has been tried. Saying so is all that is left.
			return Decision{Step: StepUnconfirmed,
				Reason: "the beam is still on after a feed hold, a spindle stop and a soft reset"}
		}
		if s.Tried.SpindleStop {
			return Decision{Step: StepSoftReset,
				Reason: "the beam stayed on after a feed hold and a spindle stop; soft reset"}
		}
		return Decision{Step: StepSoftReset,
			Reason: "the beam is on and the machine is not in a hold, so nothing gentler reaches it; soft reset"}

	case grbl.BeamOff:
		// The default policy ends a disconnected job outright: a soft reset
		// switches the output off whatever $32 says, and the job is over
		// anyway because the client that was streaming it has gone.
		if s.Trigger == TriggerClientGone && s.Policy == DisconnectReset && !s.Tried.SoftReset {
			return Decision{Step: StepSoftReset, WaitForRest: true,
				Reason: "soft reset after the client disconnected mid-job"}
		}
		return Decision{Step: StepNone}

	default:
		// The controller has not said. With laser mode off that is not an open
		// question: the output is a spindle, and a feed hold does not stop a
		// spindle.
		if s.LaserMode == grbl.SettingOff && !s.Tried.SoftReset {
			return Decision{Step: StepSoftReset,
				Reason: "laser mode ($32) is off, so the feed hold did not switch the beam off; soft reset"}
		}
		if s.Trigger == TriggerClientGone && s.Policy == DisconnectReset && !s.Tried.SoftReset {
			return Decision{Step: StepSoftReset, WaitForRest: true,
				Reason: "soft reset after the client disconnected mid-job"}
		}
		if s.Tried.SoftReset {
			return Decision{Step: StepNone}
		}
		// Ending a job on an absence of evidence is its own kind of
		// unreliable, and on_disconnect=reset is there for anyone who would
		// rather.
		return Decision{Step: StepUnconfirmed,
			Reason: "the controller never reported whether the beam is off"}
	}
}
