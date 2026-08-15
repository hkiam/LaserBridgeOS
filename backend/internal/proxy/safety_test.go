package proxy

import (
	"testing"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/grbl"
)

// The whole ladder as a table. Each row is a situation an operator could
// actually be in, and the step it should produce - including the ones that
// were only found by correcting a simulator.
func TestDecide(t *testing.T) {
	tests := []struct {
		name string
		in   Situation
		want Step
		rest bool
	}{
		{
			"none never commands the machine",
			Situation{Policy: DisconnectNone, Trigger: TriggerClientGone,
				State: grbl.StateRun, Beam: grbl.BeamOn},
			StepNone, false,
		},
		{
			"a cutting machine is held first",
			Situation{Policy: DisconnectHold, Trigger: TriggerClientGone,
				State: grbl.StateRun, Beam: grbl.BeamOn},
			StepFeedHold, false,
		},
		{
			"held, and the controller says the beam is on",
			Situation{Policy: DisconnectHold, Trigger: TriggerClientGone,
				State: grbl.StateHold, Beam: grbl.BeamOn, Tried: Tried{FeedHold: true}},
			StepSpindleStop, false,
		},
		{
			"the spindle stop did not take",
			Situation{Policy: DisconnectHold, Trigger: TriggerClientGone,
				State: grbl.StateHold, Beam: grbl.BeamOn,
				Tried: Tried{FeedHold: true, SpindleStop: true}},
			StepSoftReset, false,
		},
		{
			// The case a wrong simulator hid: GRBL ignores a feed hold from
			// Idle and the override outside a hold, so neither rung reaches
			// this machine and trying them only prolongs the burn.
			"standing still with the beam on goes straight to a reset",
			Situation{Policy: DisconnectHold, Trigger: TriggerBeamUnattended,
				State: grbl.StateIdle, Beam: grbl.BeamOn},
			StepSoftReset, false,
		},
		{
			"an alarmed machine with the beam on is no different",
			Situation{Policy: DisconnectHold, Trigger: TriggerBeamStationary,
				State: grbl.StateAlarm, Beam: grbl.BeamOn},
			StepSoftReset, false,
		},
		{
			"held with the beam confirmed off is finished",
			Situation{Policy: DisconnectHold, Trigger: TriggerClientGone,
				State: grbl.StateHold, Beam: grbl.BeamOff, Tried: Tried{FeedHold: true}},
			StepNone, false,
		},
		{
			"the default ends a disconnected job even with the beam off",
			Situation{Policy: DisconnectReset, Trigger: TriggerClientGone,
				State: grbl.StateHold, Beam: grbl.BeamOff, Tried: Tried{FeedHold: true}},
			StepSoftReset, true,
		},
		{
			// A watchdog is not a policy. It stops a beam; it does not end
			// jobs that are still being driven.
			"the watchdog does not reset a machine whose beam went off",
			Situation{Policy: DisconnectReset, Trigger: TriggerBeamStationary,
				State: grbl.StateHold, Beam: grbl.BeamOff, Tried: Tried{FeedHold: true}},
			StepNone, false,
		},
		{
			"spindle mode is not an open question",
			Situation{Policy: DisconnectHold, Trigger: TriggerClientGone,
				State: grbl.StateHold, Beam: grbl.BeamUnknown, LaserMode: grbl.SettingOff,
				Tried: Tried{FeedHold: true}},
			StepSoftReset, false,
		},
		{
			"a silent controller in laser mode is recorded, not reset",
			Situation{Policy: DisconnectHold, Trigger: TriggerClientGone,
				State: grbl.StateHold, Beam: grbl.BeamUnknown, LaserMode: grbl.SettingOn,
				Tried: Tried{FeedHold: true}},
			StepUnconfirmed, false,
		},
		{
			"a silent controller with nothing known is recorded, not reset",
			Situation{Policy: DisconnectHold, Trigger: TriggerBeamUnattended,
				State: grbl.StateHold, Beam: grbl.BeamUnknown,
				Tried: Tried{FeedHold: true}},
			StepUnconfirmed, false,
		},
		{
			"an idle machine with the beam off needs nothing",
			Situation{Policy: DisconnectReset, Trigger: TriggerClientGone,
				State: grbl.StateIdle, Beam: grbl.BeamOff},
			StepNone, false,
		},
		{
			// Whether it is a pierce or a hang, the bridge cannot tell; what
			// it must not do is nothing.
			"a machine that reports Run but is not moving still gets held",
			Situation{Policy: DisconnectHold, Trigger: TriggerBeamStationary,
				State: grbl.StateRun, Beam: grbl.BeamOn},
			StepFeedHold, false,
		},
		{
			"firmware nobody has heard of is treated as moving",
			Situation{Policy: DisconnectHold, Trigger: TriggerBeamUnattended,
				State: grbl.MachineState("Ludicrous"), Beam: grbl.BeamOn},
			StepFeedHold, false,
		},
		{
			"once everything has been tried, saying so is all that is left",
			Situation{Policy: DisconnectHold, Trigger: TriggerBeamUnattended,
				State: grbl.StateHold, Beam: grbl.BeamOn,
				Tried: Tried{FeedHold: true, SpindleStop: true, SoftReset: true}},
			StepUnconfirmed, false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Decide(test.in)
			if got.Step != test.want {
				t.Errorf("step = %v, want %v (reason %q)", got.Step, test.want, got.Reason)
			}
			if got.WaitForRest != test.rest {
				t.Errorf("WaitForRest = %v, want %v", got.WaitForRest, test.rest)
			}
			if got.Step != StepNone && got.Reason == "" {
				t.Error("no reason given; the record would say nothing")
			}
		})
	}
}

func TestDecideAlwaysTerminates(t *testing.T) {
	// The executor loops until StepNone or a terminal step. A situation that
	// kept recommending the same rung would loop forever with a laser on, so
	// every path has to run out - which is what the Tried flags are for.
	for _, state := range []grbl.MachineState{
		grbl.StateIdle, grbl.StateRun, grbl.StateHold, grbl.StateAlarm,
		grbl.StateJog, grbl.StateDoor, grbl.MachineState("Wat"), grbl.StateUnknown,
	} {
		for _, beam := range []grbl.Beam{grbl.BeamUnknown, grbl.BeamOn, grbl.BeamOff} {
			for _, mode := range []grbl.Setting{grbl.SettingUnknown, grbl.SettingOn, grbl.SettingOff} {
				for _, policy := range []string{DisconnectHold, DisconnectReset} {
					for _, trigger := range []Trigger{TriggerClientGone, TriggerBeamUnattended, TriggerBeamStationary} {
						situation := Situation{Policy: policy, Trigger: trigger,
							State: state, Beam: beam, LaserMode: mode}
						steps := 0
						for {
							decision := Decide(situation)
							if decision.Step == StepNone || decision.Step == StepUnconfirmed {
								break
							}
							switch decision.Step {
							case StepFeedHold:
								situation.Tried.FeedHold = true
							case StepSpindleStop:
								situation.Tried.SpindleStop = true
							case StepSoftReset:
								situation.Tried.SoftReset = true
							}
							if steps++; steps > 4 {
								t.Fatalf("no end for %s/%s/%s/%s/%s", policy, trigger, state, beam, mode)
							}
						}
					}
				}
			}
		}
	}
}

func TestEveryLadderEndsWithTheBeamAddressed(t *testing.T) {
	// For every situation where the controller says the beam is on, the ladder
	// must reach a soft reset - the one command whose effect on the output
	// does not depend on configuration. Anything less would be a path that
	// leaves a laser burning.
	for _, state := range []grbl.MachineState{
		grbl.StateIdle, grbl.StateRun, grbl.StateHold, grbl.StateAlarm, grbl.StateJog,
	} {
		for _, policy := range []string{DisconnectHold, DisconnectReset} {
			situation := Situation{Policy: policy, Trigger: TriggerBeamUnattended,
				State: state, Beam: grbl.BeamOn}
			reset := false
			for i := 0; i < 5; i++ {
				decision := Decide(situation)
				switch decision.Step {
				case StepFeedHold:
					situation.Tried.FeedHold = true
					// A hold moves a moving machine into HOLD.
					if !situation.State.AtRest() {
						situation.State = grbl.StateHold
					}
				case StepSpindleStop:
					situation.Tried.SpindleStop = true
				case StepSoftReset:
					reset = true
				}
				if decision.Step == StepSoftReset || decision.Step == StepNone || decision.Step == StepUnconfirmed {
					break
				}
			}
			if !reset {
				t.Errorf("%s/%s with the beam on never reaches a soft reset", policy, state)
			}
		}
	}
}
