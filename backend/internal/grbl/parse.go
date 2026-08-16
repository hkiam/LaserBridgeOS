// Package grbl reads what a GRBL controller says.
//
// It only ever observes. The bridge forwards every byte unchanged and hands a
// copy here, so nothing in this package can alter what reaches the client or
// the machine. That separation is deliberate: a parser that got a message
// wrong should produce a wrong reading on a status page, never a wrong byte on
// the wire.
//
// The dialect is GRBL 1.1, which answers in three shapes:
//
//	<Idle|MPos:0.000,0.000,0.000|FS:0,0|WCO:0.000,0.000,0.000>
//	ok
//	error:9   ALARM:1   [MSG:Caution: Unlocked]   Grbl 1.1f ['$' for help]
//
// Older firmware reports WPos instead of MPos and omits fields; unknown fields
// are ignored rather than treated as an error, because a controller saying
// something new is not a malfunction.
package grbl

import (
	"strconv"
	"strings"
)

// MachineState is GRBL's own word for what the controller is doing. The
// strings are GRBL's, not ours, so an unfamiliar state from newer firmware
// survives the trip to the web interface intact.
type MachineState string

const (
	StateUnknown MachineState = ""
	StateIdle    MachineState = "Idle"
	StateRun     MachineState = "Run"
	StateHold    MachineState = "Hold"
	StateJog     MachineState = "Jog"
	StateAlarm   MachineState = "Alarm"
	StateDoor    MachineState = "Door"
	StateCheck   MachineState = "Check"
	StateHome    MachineState = "Home"
	StateSleep   MachineState = "Sleep"
)

// Moving reports whether the machine is doing something that must not be
// interrupted casually. Anything unrecognised counts as moving: when in doubt
// about a laser, assume the beam is on.
func (s MachineState) Moving() bool {
	switch s {
	case StateIdle, StateAlarm, StateSleep, StateCheck, StateUnknown:
		return false
	default:
		return true
	}
}

// AtRest reports whether the axes have actually stopped.
//
// This is not the opposite of Moving, and the difference matters. A machine on
// feed hold is at rest but must not be treated as idle - there is still a job
// in the planner buffer waiting to resume. Both questions get asked, about the
// same state, for different reasons: Moving decides whether to intervene,
// AtRest decides whether it is safe to send a soft reset, which loses the
// position if the axes are still turning.
//
// An unfamiliar state is not at rest, for the same reason it counts as moving.
func (s MachineState) AtRest() bool {
	switch s {
	case StateIdle, StateHold, StateAlarm, StateSleep, StateCheck, StateDoor:
		return true
	default:
		return false
	}
}

// Position is a machine coordinate triple in millimetres.
type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// Report is one parsed line from the controller.
type Report struct {
	// Kind is "status", "ok", "error", "alarm", "message", "welcome" or
	// "unknown".
	Kind string
	// State and Position are set for a status report.
	State    MachineState
	Position Position
	// HasPosition distinguishes a report without coordinates from one that
	// genuinely sits at the origin.
	HasPosition bool
	Feed        float64
	Spindle     float64
	// Code carries the number of an error: or ALARM: line.
	Code int
	// Text is the line as received, minus the line ending.
	Text string
	// HasOverrides is set when a status report carried its Ov: field. GRBL
	// sends Ov: and A: together, every tenth report or so and whenever either
	// changes, so a report with Ov: and no accessory is positive evidence that
	// the spindle and coolant outputs are off - and a report without Ov: says
	// nothing about them either way.
	HasOverrides bool
	// Accessory is the A: field: S for spindle clockwise, C for counter-
	// clockwise, F for flood, M for mist. On a laser, S or C means the beam is
	// on. GRBL reads this from the actual output, not from the modal state.
	Accessory string
	// Setting and SettingValue carry one line of a $$ dump.
	Setting      string
	SettingValue string
}

// Parse reads one line. The line should already have its ending removed.
func Parse(line string) Report {
	line = strings.TrimSpace(line)
	report := Report{Kind: "unknown", Text: line}
	switch {
	case line == "":
		return Report{Kind: "unknown"}
	case line == "ok":
		report.Kind = "ok"
	case strings.HasPrefix(line, "error:"):
		report.Kind = "error"
		report.Code, _ = strconv.Atoi(strings.TrimPrefix(line, "error:"))
	case strings.HasPrefix(line, "ALARM:"):
		report.Kind = "alarm"
		report.State = StateAlarm
		report.Code, _ = strconv.Atoi(strings.TrimPrefix(line, "ALARM:"))
	case strings.HasPrefix(line, "Grbl "):
		// The welcome banner. It arrives after every reset, which is how the
		// bridge learns that the controller restarted underneath it.
		report.Kind = "welcome"
	case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
		report.Kind = "message"
	case strings.HasPrefix(line, "<") && strings.HasSuffix(line, ">"):
		parseStatus(strings.TrimSuffix(strings.TrimPrefix(line, "<"), ">"), &report)
	case strings.HasPrefix(line, "$") && strings.Contains(line, "="):
		// One line of a $$ dump, such as "$32=1". The appliance cares about
		// exactly one of them, but they all parse the same way.
		report.Kind = "setting"
		report.Setting, report.SettingValue, _ = strings.Cut(line, "=")
	}
	return report
}

// LaserMode is GRBL setting $32, and the answer to the question that decides
// whether a feed hold switches the beam off.
const LaserMode = "$32"

// SpindleMax is GRBL's maximum spindle speed. It is what a percentage of laser
// power is a percentage of, and assuming GRBL's default of 1000 when the
// controller says otherwise would aim with the wrong power.
const SpindleMax = "$30"

func parseStatus(body string, report *Report) {
	fields := strings.Split(body, "|")
	if len(fields) == 0 {
		return
	}
	report.Kind = "status"
	// The state may carry a substate, as in "Hold:0" or "Door:1". The substate
	// says why; for deciding whether the machine is busy, the part before the
	// colon is what matters.
	report.State = MachineState(strings.SplitN(fields[0], ":", 2)[0])

	for _, field := range fields[1:] {
		name, value, found := strings.Cut(field, ":")
		if !found {
			continue
		}
		switch name {
		case "MPos", "WPos":
			// MPos is machine coordinates, WPos is work coordinates; a
			// controller sends one or the other, never both.
			if position, ok := parsePosition(value); ok {
				report.Position = position
				report.HasPosition = true
			}
		case "FS":
			// Feed and spindle speed, in that order. On a laser, spindle speed
			// is the laser power.
			numbers := parseNumbers(value)
			if len(numbers) > 0 {
				report.Feed = numbers[0]
			}
			if len(numbers) > 1 {
				report.Spindle = numbers[1]
			}
		case "F":
			if numbers := parseNumbers(value); len(numbers) > 0 {
				report.Feed = numbers[0]
			}
		case "Ov":
			report.HasOverrides = true
		case "A":
			report.Accessory = value
		}
	}
}

func parsePosition(value string) (Position, bool) {
	numbers := parseNumbers(value)
	if len(numbers) < 2 {
		return Position{}, false
	}
	position := Position{X: numbers[0], Y: numbers[1]}
	if len(numbers) > 2 {
		position.Z = numbers[2]
	}
	return position, true
}

func parseNumbers(value string) []float64 {
	parts := strings.Split(value, ",")
	numbers := make([]float64, 0, len(parts))
	for _, part := range parts {
		number, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return nil
		}
		numbers = append(numbers, number)
	}
	return numbers
}
