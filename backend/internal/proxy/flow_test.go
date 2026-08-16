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
		if err := bridge.Do(CommandJog, Jog{Axis: "X", Distance: 1, Feed: 1000}, 0, "holder"); err != nil {
			t.Fatalf("jog %d was refused: %v", i+1, err)
		}
	}
	err := bridge.Do(CommandJog, Jog{Axis: "X", Distance: 1, Feed: 1000}, 0, "holder")
	if err != ErrStillWorking {
		t.Fatalf("err = %v, want the pad to wait for the controller", err)
	}

	// An "ok" releases exactly one slot, so the next jog goes through.
	if _, err := controller.Write([]byte("ok\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		return bridge.Do(CommandJog, Jog{Axis: "X", Distance: 1, Feed: 1000}, 0, "holder") == nil
	}, "the answered slot to be reusable")
}

func TestAStopStillWorksWhileTheQueueIsFull(t *testing.T) {
	// Stop is a real-time byte and does not queue, which matters precisely when
	// the queue is full: that is when somebody wants it.
	controller, _, bridge := startBridgeWith(t, func(c *Config) {})
	waitFor(t, func() bool { return bridge.Status().Device != "" }, "the port to open")
	for i := 0; i < outstandingLines; i++ {
		_ = bridge.Do(CommandJog, Jog{Axis: "X", Distance: 1, Feed: 1000}, 0, "holder")
	}
	if err := bridge.Do(CommandStop, Jog{}, 0, "holder"); err != nil {
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

	if err := bridge.Do(CommandAim, Jog{}, 2, "session one"); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Do(CommandAim, Jog{}, 40, "session two"); err != ErrSomebodyElseIsAiming {
		t.Fatalf("err = %v, want the second session refused", err)
	}
	if err := bridge.Do(CommandAimOff, Jog{}, 0, "session two"); err != ErrSomebodyElseIsAiming {
		t.Fatalf("err = %v, want the second session unable to let go of it either", err)
	}
	// The holder may renew and may let go.
	if _, err := bridge.aim.hold("session one", 2, time.Now()); err != nil {
		t.Fatalf("the holder could not renew: %v", err)
	}
	if err := bridge.Do(CommandAimOff, Jog{}, 0, "session one"); err != nil {
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

	sim.set("Jog", false)
	time.Sleep(700 * time.Millisecond)
	if job := bridge.Status().Job; job.Running {
		t.Error("jogging started a job; an update would be refused for fifteen seconds after every arrow press")
	}
	for _, event := range bridge.Journal().Recent(0) {
		if strings.HasPrefix(event.Text, "job ") && strings.Contains(event.Text, "0 lines") {
			t.Errorf("a jog was written into the record as a job: %q", event.Text)
		}
	}
}
