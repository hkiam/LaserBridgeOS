package proxy

import (
	"net"
	"strings"
	"testing"
	"time"
)

// The opening questions, and the one thing that makes them safe to ask.
//
// $I and $$ are queued commands and GRBL answers them with "ok". A sender
// counts those to know how much of the 128-byte receive buffer is free, so one
// it never earned makes it write past the end of the buffer - characters are
// dropped and the machine runs G-code nobody sent. ADR 0013 therefore forbids
// injecting anything answerable with "ok", and rejected "only when no client is
// attached" because a client can connect fifty milliseconds later.
//
// What makes it safe here is the order of events rather than a check: the
// questions are asked in the window after the port opens, and no connection is
// handed to serveClient until they are finished.
func TestTheControllerIsAskedWhatItIsBeforeAnyoneIsServed(t *testing.T) {
	controller, address, bridge := startBridgeWith(t, func(c *Config) {
		c.IdlePoll = 100 * time.Millisecond
	})
	sim := &controllerSim{t: t, controller: controller, state: "Idle", overrides: true, laserMode: "0"}
	done := make(chan struct{})
	go sim.run(done)
	t.Cleanup(func() { close(done) })

	// Waiting for the record rather than for the reading: the note is written
	// when the probe notices the answer, which is a tick after the answer
	// arrived.
	waitFor(t, func() bool {
		for _, event := range bridge.Journal().Recent(0) {
			if strings.Contains(event.Text, "laser mode ($32) is OFF") {
				return true
			}
		}
		return false
	}, "$32 to be learned unasked and written down")

	machine := bridge.Status().Machine
	if machine.LaserMode != "off" {
		t.Errorf("laser mode = %q, want off", machine.LaserMode)
	}
	if machine.Firmware != "1.1h.20190825" {
		t.Errorf("firmware = %q, want it kept out of LastMessage's way", machine.Firmware)
	}
	if machine.Options != "V,15,128" {
		t.Errorf("options = %q", machine.Options)
	}
	// A client served after all that starts with a clean slate: the dump was
	// forwarded to nobody, because there was nobody.
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	waitForState(bridge, StateClientConnected)
	if err := client.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 4096)
	n, _ := client.Read(buffer)
	if strings.Contains(string(buffer[:n]), "ok") {
		t.Errorf("a client was handed an ok it never earned: %q", buffer[:n])
	}
}

func TestTheProbeSurvivesTheResetItCauses(t *testing.T) {
	// What the first appliance to run this actually did, every boot.
	//
	// Opening the serial port toggles DTR and resets an Arduino-based
	// controller. It then says nothing at all while it boots and announces
	// itself about two seconds later. The probe waited a second and a half for
	// a status report, gave up, and the appliance never learned $32 - defeated
	// by a reset it had caused itself.
	controller, _, bridge := startBridgeWith(t, func(c *Config) {
		c.IdlePoll = 100 * time.Millisecond
		// The real budget: this is the test that exists for it.
		c.ProbeIdentify = probeIdentify
	})
	sim := &controllerSim{t: t, controller: controller, state: "Idle", overrides: true, laserMode: "0"}
	done := make(chan struct{})
	go func() {
		// Silent while it "reboots", then the banner, then normal service.
		time.Sleep(2 * time.Second)
		_, _ = controller.Write([]byte("Grbl 1.1h ['$' for help]\r\n"))
		sim.run(done)
	}()
	t.Cleanup(func() { close(done) })

	waitFor(t, func() bool {
		for _, event := range bridge.Journal().Recent(0) {
			if strings.Contains(event.Text, "laser mode ($32) is OFF") {
				return true
			}
		}
		return false
	}, "the controller to be identified despite the reset")
}

func TestNothingIsAskedOfSomethingThatIsNotGRBL(t *testing.T) {
	// $$ is meaningless to a Ruida board and there is no telling what it means
	// to one nobody has tried. A device that has not answered like GRBL is not
	// spoken to beyond the status request the watchdog sends anyway.
	controller, _, bridge := startBridgeWith(t, func(c *Config) {
		c.IdlePoll = 100 * time.Millisecond
	})
	// Answers, but with something that is not a status report.
	go func() {
		buffer := make([]byte, 64)
		for {
			n, err := controller.Read(buffer)
			if err != nil {
				return
			}
			for range buffer[:n] {
				_, _ = controller.Write([]byte("\xff\xfe garbage\r\n"))
			}
		}
	}()

	waitFor(t, func() bool { return bridge.Status().Gibberish }, "the answer to be recognised as not GRBL")
	time.Sleep(500 * time.Millisecond)
	if mode := bridge.Status().Machine.LaserMode; mode != "" {
		t.Errorf("laser mode = %q; something was asked of a device that is not GRBL", mode)
	}
}
