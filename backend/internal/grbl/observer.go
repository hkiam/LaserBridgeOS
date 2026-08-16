package grbl

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxLine bounds how much a single unterminated line may accumulate. A
// controller answering at 250000 baud with a stuck line ending would otherwise
// grow this buffer without limit on a machine with 512 MB of RAM. GRBL's
// longest real line - a full $$ settings dump entry or a status report with
// every optional field - is well under this.
const maxLine = 512

// Observer turns the controller's byte stream into reports. It is fed copies
// of what the bridge forwards, so a client's traffic is never delayed by
// parsing and never altered by it.
//
// Bytes arrive in whatever chunks the serial port produces, which has nothing
// to do with where lines end: one read can hold half a report, or three.
type Observer struct {
	mu      sync.Mutex
	line    strings.Builder
	dropped bool
	// garbage counts what arrived and made no sense as GRBL. At the wrong
	// baud rate that is everything, which is the one case worth telling
	// somebody about.
	garbage int
	// recognised counts lines that did parse.
	recognised int

	machine Machine
	// lastReport is when the controller last said anything, at better than the
	// one-second resolution the JSON carries. A watchdog deciding whether to
	// ask a question cannot work in whole seconds.
	lastReport time.Time
	// now is swappable so tests need not sleep.
	now func() time.Time

	// OnReport is called for lines worth remembering - alarms, errors,
	// messages, the welcome banner - and for plain "ok", which is worth
	// nothing to look at but answers for exactly one line the client sent.
	// The machine is passed as it stands afterwards.
	// It is called outside the observer's lock, so it may ask questions back.
	// Status reports do not go through it: there are several a second during a
	// job and nothing is learned by recording them.
	OnReport func(Report, Machine)
}

// Beam is what the controller says about its own laser output.
//
// Three values, and the third one matters as much as the other two. Deciding
// what to do with an unattended machine on the assumption that the beam is off
// is exactly the mistake worth designing against, so "not known" is a value
// rather than a default of "off".
type Beam string

const (
	BeamUnknown Beam = ""
	BeamOn      Beam = "on"
	BeamOff     Beam = "off"
)

// Setting is a GRBL setting whose value the appliance has seen, or has not.
type Setting string

const (
	SettingUnknown Setting = ""
	SettingOn      Setting = "on"
	SettingOff     Setting = "off"
)

// Machine is the current reading of the controller. Every field may be absent:
// a controller that has said nothing yet has no state, and asking it to
// speak is the client's business, not the bridge's.
type Machine struct {
	State       MachineState `json:"state,omitempty"`
	Position    Position     `json:"position"`
	HasPosition bool         `json:"has_position"`
	Feed        float64      `json:"feed"`
	Spindle     float64      `json:"spindle"`
	// AlarmCode is the last ALARM: number, LastError the last error: number.
	AlarmCode int `json:"alarm_code,omitempty"`
	LastError int `json:"last_error_code,omitempty"`
	// LastMessage is the most recent [MSG:...] line, which is where GRBL
	// explains itself in words.
	LastMessage string `json:"last_message,omitempty"`
	// LastReportUnix is when the controller last said anything at all. A
	// controller that has gone quiet is the interesting case, and only a
	// timestamp can show it.
	LastReportUnix int64 `json:"last_report_unix,omitempty"`
	// Resets counts welcome banners seen. The controller resetting underneath
	// a running job is worth noticing.
	Resets int `json:"resets"`
	// Beam is whether the laser output is on, taken from the accessory field
	// of a status report - which GRBL fills from the actual output rather than
	// from the modal state, so it is evidence and not inference.
	//
	// It is evidence with a date on it, and the date matters. GRBL sends the
	// accessory field only alongside Ov:, which is every tenth report or so
	// plus whenever either changes, and it fills it from the output as it is at
	// that instant. In laser mode with M4 the output is genuinely off during
	// rapids and at zero-power moments, so a report can truthfully say "nothing
	// on" a fraction of a second before the beam is burning again. Reading this
	// as a state - "the laser is off" - while a job is running says more than
	// the controller did, which is how a cutting machine came to be shown as
	// off.
	Beam Beam `json:"beam"`
	// BeamUnix is when the controller last said anything about its outputs. A
	// reading without it is a claim without a date.
	BeamUnix int64 `json:"beam_unix,omitempty"`
	// Reports counts status reports seen; BeamReports is what that count was
	// when the outputs were last mentioned. The difference is how stale the
	// reading is measured in the controller's own terms, which is the only
	// measure that means anything: GRBL prints the Ov: block every tenth
	// report when idle and every twentieth while moving, so "two seconds old"
	// says nothing without knowing how much was said in those two seconds.
	Reports     int64 `json:"reports,omitempty"`
	BeamReports int64 `json:"beam_reports,omitempty"`
	// LaserMode is GRBL setting $32, seen in a settings dump. It decides
	// whether a feed hold switches the beam off: with laser mode off, the
	// output is a spindle, and a feed hold deliberately leaves a spindle
	// running.
	LaserMode Setting `json:"laser_mode"`
	// Firmware and Options are what the controller says it is, from the [VER:]
	// and [OPT:] lines it answers $I with.
	//
	// They were being seen and thrown away. Both arrive as bracketed messages,
	// and LastMessage is a single slot that the next one overwrites - so on a
	// real appliance the firmware string survived for about a millisecond
	// before a [G54:] line replaced it. Everything this bridge does about an
	// unattended laser rests on how a particular GRBL behaves, which makes the
	// version that told us so worth more than one slot.
	Firmware string `json:"firmware,omitempty"`
	Options  string `json:"options,omitempty"`
	// SpindleMax is $30, what an S value of full power is on this controller.
	SpindleMax int `json:"spindle_max,omitempty"`
	// Homing is $22, SoftLimits $20 and HardLimits $21. They decide what the
	// steering may honestly offer: a Home button on a machine with no homing
	// cycle answers error:5, and a jog on a machine with no limits at all is
	// bounded by nothing but the rails.
	Homing     Setting `json:"homing,omitempty"`
	SoftLimits Setting `json:"soft_limits,omitempty"`
	HardLimits Setting `json:"hard_limits,omitempty"`
}

func NewObserver() *Observer {
	return &Observer{now: time.Now}
}

// Write feeds bytes from the controller. It never fails and never blocks on
// anything but its own lock, because the caller is on the path between the
// machine and the client.
func (o *Observer) Write(data []byte) {
	o.mu.Lock()
	var notable []Report
	for _, b := range data {
		switch b {
		case '\n', '\r':
			if report, ok := o.flush(); ok {
				notable = append(notable, report)
			}
		default:
			if o.line.Len() >= maxLine {
				// Keep consuming, but stop remembering: a line this long is not
				// something GRBL sends, so the stream is either noise or the
				// wrong baud rate.
				o.dropped = true
				o.garbage++
				continue
			}
			o.line.WriteByte(b)
		}
	}
	machine := o.machine
	o.mu.Unlock()

	// Outside the lock: a listener that wants to know the position as well
	// would otherwise deadlock asking for it.
	if o.OnReport != nil {
		for _, report := range notable {
			o.OnReport(report, machine)
		}
	}
}

// flush parses the line just completed and reports whether it is one a
// listener should hear about.
func (o *Observer) flush() (Report, bool) {
	text := o.line.String()
	o.line.Reset()
	dropped := o.dropped
	o.dropped = false
	if text == "" || dropped {
		return Report{}, false
	}
	report := Parse(text)
	o.apply(report)
	switch report.Kind {
	case "alarm", "error", "welcome", "message", "setting":
		return report, true
	case "ok":
		// Not interesting in itself, but it answers for exactly one line the
		// client sent, and somebody is counting.
		return report, true
	case "unknown":
		// Not GRBL. Counted rather than announced, because at the wrong baud
		// rate every line looks like this and the point is the total.
		o.garbage++
	}
	return Report{}, false
}

func (o *Observer) apply(report Report) {
	if report.Kind == "unknown" && report.Text == "" {
		return
	}
	if report.Kind != "unknown" {
		o.recognised++
	}
	o.lastReport = o.now()
	o.machine.LastReportUnix = o.lastReport.Unix()
	switch report.Kind {
	case "status":
		o.machine.State = report.State
		o.machine.Feed = report.Feed
		o.machine.Spindle = report.Spindle
		if report.HasPosition {
			o.machine.Position = report.Position
			o.machine.HasPosition = true
		}
		if report.State != StateAlarm {
			o.machine.AlarmCode = 0
		}
		// Only a report that carried Ov: says anything about the outputs, and
		// then it says it definitively: GRBL emits the accessory field beside
		// Ov: whenever anything is on, so Ov: without it means everything is
		// off. A report without Ov: leaves the previous reading standing
		// rather than being read as "off".
		o.machine.Reports++
		if report.HasOverrides {
			if strings.ContainsAny(report.Accessory, "SC") {
				o.machine.Beam = BeamOn
			} else {
				o.machine.Beam = BeamOff
			}
			o.machine.BeamUnix = o.lastReport.Unix()
			o.machine.BeamReports = o.machine.Reports
		}
	case "setting":
		if report.Setting == SpindleMax {
			if value, err := strconv.Atoi(strings.TrimSpace(report.SettingValue)); err == nil && value > 0 {
				o.machine.SpindleMax = value
			}
		}
		switch report.Setting {
		case HomingCycle:
			o.machine.Homing = onOff(report.SettingValue)
		case SoftLimits:
			o.machine.SoftLimits = onOff(report.SettingValue)
		case HardLimits:
			o.machine.HardLimits = onOff(report.SettingValue)
		}
		if report.Setting == LaserMode {
			if strings.TrimSpace(report.SettingValue) == "0" {
				o.machine.LaserMode = SettingOff
			} else {
				o.machine.LaserMode = SettingOn
			}
		}
	case "alarm":
		o.machine.State = StateAlarm
		o.machine.AlarmCode = report.Code
	case "error":
		o.machine.LastError = report.Code
	case "message":
		message := strings.Trim(report.Text, "[]")
		o.machine.LastMessage = message
		// Kept in their own fields, because the next bracketed line is a
		// fraction of a second away and there is only one LastMessage.
		switch {
		case strings.HasPrefix(message, "VER:"):
			o.machine.Firmware = strings.TrimSuffix(strings.TrimPrefix(message, "VER:"), ":")
		case strings.HasPrefix(message, "OPT:"):
			o.machine.Options = strings.TrimPrefix(message, "OPT:")
		}
	case "welcome":
		// A reset clears everything the controller knew, so the reading has to
		// forget it too rather than show coordinates from before the reset.
		// A reset turns the outputs off - that is what mc_reset does - and
		// $32 is a stored setting that a reset does not change, so both are
		// carried over rather than forgotten with everything else.
		o.machine = Machine{
			Resets:         o.machine.Resets + 1,
			LastReportUnix: o.machine.LastReportUnix,
			State:          StateUnknown,
			Beam:           BeamOff,
			// A banner is the controller saying its outputs are off, now - so
			// this reading is as fresh as the banner is.
			BeamUnix:    o.lastReport.Unix(),
			Reports:     o.machine.Reports,
			BeamReports: o.machine.Reports,
			// A reset changes neither the firmware nor the stored settings, so
			// what the controller told us about itself still holds.
			LaserMode:  o.machine.LaserMode,
			Firmware:   o.machine.Firmware,
			Options:    o.machine.Options,
			SpindleMax: o.machine.SpindleMax,
			Homing:     o.machine.Homing,
			SoftLimits: o.machine.SoftLimits,
			HardLimits: o.machine.HardLimits,
		}
	}
}

// onOff reads one of GRBL's boolean settings. Anything that is not zero is on,
// which is how GRBL itself treats them.
func onOff(value string) Setting {
	if strings.TrimSpace(value) == "0" {
		return SettingOff
	}
	return SettingOn
}

// Machine returns the current reading.
func (o *Observer) Machine() Machine {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.machine
}

// Silent reports whether the controller has said nothing for longer than the
// given span. A controller that has never spoken is not silent - it may simply
// not have been asked.
func (o *Observer) Silent(for_ time.Duration) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.machine.LastReportUnix == 0 {
		return false
	}
	return o.now().Sub(time.Unix(o.machine.LastReportUnix, 0)) > for_
}

// Quiet reports whether nothing has been heard for the given span. Unlike
// Silent, a controller that has never spoken counts as quiet: the caller here
// is deciding whether to ask, not whether to worry.
func (o *Observer) Quiet(for_ time.Duration) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.lastReport.IsZero() {
		return true
	}
	return o.now().Sub(o.lastReport) > for_
}

// Gibberish reports whether the controller is answering with something that is
// not GRBL at all.
//
// This is what the wrong baud rate looks like from here: bytes arrive, none of
// them parse, and every one of them is noise. It is the single most common
// setup mistake and the one the appliance can spot on its own - but only once
// there is enough to judge, because a controller mid-line or a stray byte on a
// good link must not be enough to raise it.
func (o *Observer) Gibberish() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.recognised == 0 && o.garbage >= 5
}

// ForgetBeam drops what was known about the laser output, so the next question
// is answered by what the controller says now rather than by what it said
// before something was done to it.
func (o *Observer) ForgetBeam() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.machine.Beam = BeamUnknown
	o.machine.BeamUnix = 0
	o.machine.BeamReports = 0
}

// Reset forgets everything, for when the port is reopened and the reading
// would otherwise describe a controller that is no longer there.
func (o *Observer) Reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.machine = Machine{}
	o.lastReport = time.Time{}
	o.line.Reset()
	o.dropped = false
	o.garbage = 0
	o.recognised = 0
}
