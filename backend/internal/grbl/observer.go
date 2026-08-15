package grbl

import (
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

	machine Machine
	// now is swappable so tests need not sleep.
	now func() time.Time
}

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
}

func NewObserver() *Observer {
	return &Observer{now: time.Now}
}

// Write feeds bytes from the controller. It never fails and never blocks on
// anything but its own lock, because the caller is on the path between the
// machine and the client.
func (o *Observer) Write(data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, b := range data {
		switch b {
		case '\n', '\r':
			o.flush()
		default:
			if o.line.Len() >= maxLine {
				// Keep consuming, but stop remembering: a line this long is not
				// something GRBL sends, so the stream is either noise or the
				// wrong baud rate.
				o.dropped = true
				continue
			}
			o.line.WriteByte(b)
		}
	}
}

func (o *Observer) flush() {
	text := o.line.String()
	o.line.Reset()
	dropped := o.dropped
	o.dropped = false
	if text == "" || dropped {
		return
	}
	o.apply(Parse(text))
}

func (o *Observer) apply(report Report) {
	if report.Kind == "unknown" && report.Text == "" {
		return
	}
	o.machine.LastReportUnix = o.now().Unix()
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
	case "alarm":
		o.machine.State = StateAlarm
		o.machine.AlarmCode = report.Code
	case "error":
		o.machine.LastError = report.Code
	case "message":
		o.machine.LastMessage = strings.Trim(report.Text, "[]")
	case "welcome":
		// A reset clears everything the controller knew, so the reading has to
		// forget it too rather than show coordinates from before the reset.
		resets := o.machine.Resets + 1
		o.machine = Machine{Resets: resets, LastReportUnix: o.machine.LastReportUnix, State: StateUnknown}
	}
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

// Reset forgets everything, for when the port is reopened and the reading
// would otherwise describe a controller that is no longer there.
func (o *Observer) Reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.machine = Machine{}
	o.line.Reset()
	o.dropped = false
}
