package proxy

import (
	"strings"
	"testing"
	"time"
)

// The machine takes time, and the appliance has to keep books about it.
//
// GRBL answers a jog with "ok" when it is accepted into the planner, not when
// the move is finished - that is what makes continuous jogging possible. But it
// answers from a 128-byte receive buffer, and somebody tapping the pad faster
// than the controller answers will overfill it. This appliance insists a client
// counts its lines; it now counts its own.
func TestJoggingFasterThanTheControllerAnswersIsRefused(t *testing.T) {
	controller, _, bridge := startBridgeWith(t, func(c *Config) {})
	waitFor(t, func() bool { return bridge.Status().Device != "" }, "the port to open")

	// Nothing is reading the controller side, so nothing is answered.
	for i := 0; i < outstandingLines; i++ {
		if err := bridge.Do(CommandRequest{Command: CommandJog, Axis: "X", Distance: 1, Feed: 1000, Holder: "holder"}); err != nil {
			t.Fatalf("jog %d was refused: %v", i+1, err)
		}
	}
	err := bridge.Do(CommandRequest{Command: CommandJog, Axis: "X", Distance: 1, Feed: 1000, Holder: "holder"})
	if err != ErrStillWorking {
		t.Fatalf("err = %v, want the pad to wait for the controller", err)
	}

	// An "ok" releases exactly one slot, so the next jog goes through.
	if _, err := controller.Write([]byte("ok\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		return bridge.Do(CommandRequest{Command: CommandJog, Axis: "X", Distance: 1, Feed: 1000, Holder: "holder"}) == nil
	}, "the answered slot to be reusable")
}

func TestAStopStillWorksWhileTheQueueIsFull(t *testing.T) {
	// Stop is a real-time byte and does not queue, which matters precisely when
	// the queue is full: that is when somebody wants it.
	controller, _, bridge := startBridgeWith(t, func(c *Config) {})
	waitFor(t, func() bool { return bridge.Status().Device != "" }, "the port to open")
	for i := 0; i < outstandingLines; i++ {
		_ = bridge.Do(CommandRequest{Command: CommandJog, Axis: "X", Distance: 1, Feed: 1000, Holder: "holder"})
	}
	if err := bridge.Do(CommandRequest{Command: CommandStop, Holder: "holder"}); err != nil {
		t.Fatalf("Stop was refused with a full queue: %v", err)
	}
	_ = controller
}

func TestOnlyOneSessionMayHoldTheAimingBeam(t *testing.T) {
	// Two browsers, one beam. Without a holder, either could renew the other's
	// lease or take it over mid-alignment and neither would know - which is the
	// failure this appliance refuses one floor down, between two applications
	// and one laser.
	controller, _, bridge := startBridgeWith(t, func(c *Config) {})
	sim := &controllerSim{t: t, controller: controller, state: "Idle", overrides: true, laserMode: "1"}
	done := make(chan struct{})
	go sim.run(done)
	t.Cleanup(func() { close(done) })
	waitFor(t, func() bool { return bridge.Status().Device != "" }, "the port to open")

	if err := bridge.Do(CommandRequest{Command: CommandAim, Percent: 2, Holder: "session one"}); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Do(CommandRequest{Command: CommandAim, Percent: 40, Holder: "session two"}); err != ErrSomebodyElseIsAiming {
		t.Fatalf("err = %v, want the second session refused", err)
	}
	if err := bridge.Do(CommandRequest{Command: CommandAimOff, Holder: "session two"}); err != ErrSomebodyElseIsAiming {
		t.Fatalf("err = %v, want the second session unable to let go of it either", err)
	}
	// The holder may renew and may let go.
	if _, err := bridge.aim.hold("session one", 2, time.Now()); err != nil {
		t.Fatalf("the holder could not renew: %v", err)
	}
	if err := bridge.Do(CommandRequest{Command: CommandAimOff, Holder: "session one"}); err != nil {
		t.Fatalf("the holder could not let go: %v", err)
	}
}

func TestJoggingDoesNotStartAJob(t *testing.T) {
	// MachineState.Moving() is true for Jog and Home as well as Run, so without
	// a client in the definition every tap on an arrow started a "job": a line
	// in the record about three seconds and no lines, and fifteen seconds in
	// which an update would be refused.
	sim, bridge, client := startWithSim(t, true, false, func(c *Config) {
		c.OnDisconnect = DisconnectNone
		c.IdlePoll = 100 * time.Millisecond
	})
	// Send the client away; the operator is now alone at the machine.
	client.Close()
	waitFor(t, func() bool { return bridge.Status().Client == "" }, "the client to leave")
	waitFor(t, func() bool { return !bridge.Status().Job.Running }, "the client's job to end")

	// Everything from here on is the operator alone at the pad. What went into
	// the record before it is the client's own doing: this simulator reports
	// Run from the first status request, so a job legitimately began and ended
	// with the client - and on a slow machine it also ended with "0 lines",
	// because the client never sent one. Scanning the whole record for that
	// wording therefore failed on CI and passed on a laptop, which is the worst
	// way for a test to behave.
	before := len(bridge.Journal().Recent(0))

	sim.set("Jog", false)
	time.Sleep(700 * time.Millisecond)
	if job := bridge.Status().Job; job.Running {
		t.Error("jogging started a job; an update would be refused for fifteen seconds after every arrow press")
	}
	events := bridge.Journal().Recent(0)
	if before > len(events) {
		before = 0
	}
	for _, event := range events[before:] {
		if strings.HasPrefix(event.Text, "job ") {
			t.Errorf("a jog was written into the record as a job: %q", event.Text)
		}
	}
}

// Reported from the workshop: the aiming beam could not be switched on - "the
// controller has not answered the last commands yet" - while the jog pad worked
// perfectly well, and the console showed a single M5 nobody had asked for.
//
// Three faults, all in this file's subject: what answers for a line.
func TestAnErrorAnswersForALineJustLikeAnOk(t *testing.T) {
	controller, _, bridge := startBridgeWith(t, func(c *Config) {})
	waitFor(t, func() bool { return bridge.Status().Device != "" }, "the port to open")

	// A homing cycle refused with error:9 is the everyday way this happens.
	for i := 0; i < outstandingLines; i++ {
		if err := bridge.Do(CommandRequest{Command: CommandUnlock, Holder: "holder"}); err != nil {
			t.Fatalf("line %d was refused: %v", i+1, err)
		}
	}
	if err := bridge.Do(CommandRequest{Command: CommandUnlock, Holder: "holder"}); err != ErrStillWorking {
		t.Fatalf("err = %v, want the queue to be full", err)
	}
	for i := 0; i < outstandingLines; i++ {
		if _, err := controller.Write([]byte("error:9\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool {
		return bridge.Do(CommandRequest{Command: CommandUnlock, Holder: "holder"}) == nil
	}, "a refused line to release its slot")
}

func TestAResetAnswersForEveryLineAtOnce(t *testing.T) {
	controller, _, bridge := startBridgeWith(t, func(c *Config) {})
	waitFor(t, func() bool { return bridge.Status().Device != "" }, "the port to open")

	for i := 0; i < outstandingLines; i++ {
		if err := bridge.Do(CommandRequest{Command: CommandUnlock, Holder: "holder"}); err != nil {
			t.Fatalf("line %d was refused: %v", i+1, err)
		}
	}
	// The adapter was replugged, or somebody pressed Stop. Either way the
	// controller's buffer is empty and nothing in it will ever be answered.
	if _, err := controller.Write([]byte("Grbl 1.1f ['$' for help]\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		return bridge.Do(CommandRequest{Command: CommandUnlock, Holder: "holder"}) == nil
	}, "the restart to release every slot")
}

func TestAnAimThatCouldNotBeSentDoesNotHoldTheLease(t *testing.T) {
	controller, _, bridge := startBridgeWith(t, func(c *Config) {})
	waitFor(t, func() bool { return bridge.Status().Device != "" }, "the port to open")
	_ = controller

	for i := 0; i < outstandingLines; i++ {
		if err := bridge.Do(CommandRequest{Command: CommandUnlock, Holder: "holder"}); err != nil {
			t.Fatalf("line %d was refused: %v", i+1, err)
		}
	}
	if err := bridge.Do(CommandRequest{Command: CommandAim, Percent: 2, Holder: "holder"}); err != ErrStillWorking {
		t.Fatalf("err = %v, want the aiming beam refused with a full queue", err)
	}
	// The lease is taken before the beam is lit, on purpose. A send that failed
	// must give it back: otherwise the supervisor finds an expired lease three
	// seconds later and sends an M5 for a laser that was never on - which is
	// exactly the single, unexplained M5 the console showed.
	if bridge.Aiming() {
		t.Fatal("a beam that was never lit is being held")
	}
}
