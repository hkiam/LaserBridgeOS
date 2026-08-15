package grbl

import (
	"testing"
	"time"
)

func TestParseStatusReport(t *testing.T) {
	report := Parse("<Idle|MPos:1.500,-2.250,0.000|FS:0,0|WCO:0.000,0.000,0.000>")
	if report.Kind != "status" {
		t.Fatalf("kind = %q, want status", report.Kind)
	}
	if report.State != StateIdle {
		t.Errorf("state = %q, want Idle", report.State)
	}
	if !report.HasPosition || report.Position.X != 1.5 || report.Position.Y != -2.25 {
		t.Errorf("position = %+v (has=%v)", report.Position, report.HasPosition)
	}
}

func TestParseSubstateAndFeed(t *testing.T) {
	// A hold reports why it is holding; the reason must not turn the state
	// into something unrecognisable.
	report := Parse("<Hold:0|MPos:0.000,0.000,0.000|FS:500,1000>")
	if report.State != StateHold {
		t.Errorf("state = %q, want Hold", report.State)
	}
	if report.Feed != 500 || report.Spindle != 1000 {
		t.Errorf("feed/spindle = %v/%v, want 500/1000", report.Feed, report.Spindle)
	}
	if !report.State.Moving() {
		t.Error("a machine on feed hold is not idle and must count as moving")
	}
}

func TestParseWorkCoordinatesAndPartialFields(t *testing.T) {
	// Older firmware sends WPos and may omit FS entirely.
	report := Parse("<Run|WPos:10.000,20.000>")
	if report.State != StateRun || !report.HasPosition {
		t.Fatalf("report = %+v", report)
	}
	if report.Position.X != 10 || report.Position.Y != 20 || report.Position.Z != 0 {
		t.Errorf("position = %+v", report.Position)
	}
}

func TestParseLineKinds(t *testing.T) {
	for _, testcase := range []struct {
		line string
		kind string
		code int
	}{
		{"ok", "ok", 0},
		{"error:9", "error", 9},
		{"ALARM:1", "alarm", 1},
		{"[MSG:Caution: Unlocked]", "message", 0},
		{"Grbl 1.1f ['$' for help]", "welcome", 0},
		{"something else", "unknown", 0},
	} {
		report := Parse(testcase.line)
		if report.Kind != testcase.kind || report.Code != testcase.code {
			t.Errorf("%q -> kind %q code %d, want %q %d", testcase.line, report.Kind, report.Code, testcase.kind, testcase.code)
		}
	}
}

func TestUnknownStateCountsAsMoving(t *testing.T) {
	// Firmware the appliance has never heard of must not be assumed idle.
	if !MachineState("Ludicrous").Moving() {
		t.Error("an unrecognised state must count as moving")
	}
	if StateIdle.Moving() || StateAlarm.Moving() {
		t.Error("Idle and Alarm are not moving")
	}
}

func TestObserverReassemblesSplitLines(t *testing.T) {
	// The serial port hands over whatever arrived, which has nothing to do
	// with where GRBL's lines end.
	observer := NewObserver()
	for _, chunk := range []string{"<Ru", "n|MPos:1.000,2.0", "00,3.000|FS:100,50>\r\no", "k\r\n"} {
		observer.Write([]byte(chunk))
	}
	machine := observer.Machine()
	if machine.State != StateRun {
		t.Fatalf("state = %q, want Run", machine.State)
	}
	if machine.Position.X != 1 || machine.Position.Z != 3 || machine.Spindle != 50 {
		t.Errorf("machine = %+v", machine)
	}
}

func TestObserverBoundsUnterminatedInput(t *testing.T) {
	// At the wrong baud rate the stream is noise with no line endings in it.
	observer := NewObserver()
	noise := make([]byte, 8192)
	for i := range noise {
		noise[i] = 'x'
	}
	observer.Write(noise)
	observer.mu.Lock()
	length := observer.line.Len()
	observer.mu.Unlock()
	if length > maxLine {
		t.Fatalf("buffered %d bytes, want at most %d", length, maxLine)
	}
	// And the garbage must not be reported as if the controller had said it.
	observer.Write([]byte("\n"))
	if observer.Machine().State != StateUnknown {
		t.Errorf("noise produced a state: %+v", observer.Machine())
	}
}

func TestObserverForgetsAfterReset(t *testing.T) {
	observer := NewObserver()
	observer.Write([]byte("<Run|MPos:5.000,5.000,0.000>\r\nALARM:2\r\n"))
	if machine := observer.Machine(); machine.AlarmCode != 2 {
		t.Fatalf("alarm not recorded: %+v", machine)
	}
	observer.Write([]byte("Grbl 1.1f ['$' for help]\r\n"))
	machine := observer.Machine()
	if machine.AlarmCode != 0 || machine.HasPosition || machine.State != StateUnknown {
		t.Errorf("reset did not clear the reading: %+v", machine)
	}
	if machine.Resets != 1 {
		t.Errorf("resets = %d, want 1", machine.Resets)
	}
}

func TestObserverClearsAlarmOnRecovery(t *testing.T) {
	observer := NewObserver()
	observer.Write([]byte("ALARM:1\r\n"))
	observer.Write([]byte("<Idle|MPos:0.000,0.000,0.000>\r\n"))
	if machine := observer.Machine(); machine.AlarmCode != 0 || machine.State != StateIdle {
		t.Errorf("machine = %+v, want idle with no alarm", machine)
	}
}

func TestObserverSilence(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	observer := NewObserver()
	observer.now = func() time.Time { return now }

	if observer.Silent(time.Second) {
		t.Error("a controller that has never spoken is not silent")
	}
	observer.Write([]byte("ok\r\n"))
	if observer.Silent(5 * time.Second) {
		t.Error("just spoke; not silent")
	}
	now = now.Add(30 * time.Second)
	if !observer.Silent(5 * time.Second) {
		t.Error("30s of nothing should count as silent")
	}
}

func TestAtRestIsNotTheOppositeOfMoving(t *testing.T) {
	// A machine on feed hold has stopped - that is what the hold was for - but
	// it is not idle: a job is still waiting in the planner buffer. The two
	// questions are asked for different reasons and must answer differently.
	if !StateHold.AtRest() {
		t.Error("a held machine has stopped")
	}
	if !StateHold.Moving() {
		t.Error("a held machine still has a job in it and must not be treated as idle")
	}
	for _, state := range []MachineState{StateRun, StateJog, StateHome, MachineState("Ludicrous"), StateUnknown} {
		if state.AtRest() {
			t.Errorf("%q must not count as at rest", state)
		}
	}
	for _, state := range []MachineState{StateIdle, StateAlarm, StateSleep, StateCheck, StateDoor} {
		if !state.AtRest() {
			t.Errorf("%q counts as at rest", state)
		}
	}
}
