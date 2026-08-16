package proxy

import (
	"net"
	"strings"
	"testing"
	"time"
)

// Steering from the web interface, and the rule that makes it safe to offer.
func TestTheApplianceWillNotSteerAMachineSomebodyElseIsSteering(t *testing.T) {
	sim, bridge, client := startWithSim(t, true, false, func(c *Config) {
		c.OnDisconnect = DisconnectNone
	})
	defer client.Close()

	for _, command := range []Command{CommandHome, CommandUnlock, CommandJog, CommandAim, CommandHold} {
		err := bridge.Do(command, Jog{Axis: "X", Distance: 10, Feed: 1000}, 2)
		if err == nil {
			t.Fatalf("%s was accepted while a client was connected", command)
		}
		if !strings.Contains(err.Error(), "client is connected") {
			t.Errorf("%s was refused for the wrong reason: %v", command, err)
		}
	}
	// Nothing reached the controller.
	if holds, stops, resets := sim.counts(); holds+stops+resets != 0 {
		t.Errorf("something was sent anyway: %d holds, %d stops, %d resets", holds, stops, resets)
	}
}

func TestStopIsTheOneCommandAllowedWhileSomebodyIsSteering(t *testing.T) {
	// Whoever presses it means the machine to be stopped, and leaving a sender
	// streaming into a controller that has just been reset is the hole ADR 0018
	// closed. So it takes the machine over the way the watchdog does.
	sim, bridge, client := startWithSim(t, true, false, func(c *Config) {
		c.OnDisconnect = DisconnectNone
	})
	defer client.Close()

	if err := bridge.Do(CommandStop, Jog{}, 0); err != nil {
		t.Fatalf("Stop was refused: %v", err)
	}
	waitForCounts(t, sim, 0, 0, 1)

	// And the client is gone, told why.
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	var heard strings.Builder
	buffer := make([]byte, 512)
	for {
		n, err := client.Read(buffer)
		heard.Write(buffer[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(heard.String(), "[MSG:LaserBridgeOS took over") {
		t.Errorf("the client was dropped without being told: %q", heard.String())
	}
}

func TestAJogIsRelativeAndInMillimetresEveryTime(t *testing.T) {
	controller, _, bridge := startBridgeWith(t, func(c *Config) {})
	waitFor(t, func() bool { return bridge.Status().Device != "" }, "the port to open")

	if err := bridge.Do(CommandJog, Jog{Axis: "y", Distance: -2.5, Feed: 800}, 0); err != nil {
		t.Fatal(err)
	}
	got := string(mustRead(t, controller, len("$J=G91 G21 Y-2.500 F800\n")))
	// G91 and G21 are restated on every jog: one that inherited a G20 from
	// whatever ran before it would move twenty-five times too far.
	if got != "$J=G91 G21 Y-2.500 F800\n" {
		t.Fatalf("jog line = %q", got)
	}

	for _, bad := range []Jog{
		{Axis: "A", Distance: 1, Feed: 100},
		{Axis: "X", Distance: 0, Feed: 100},
		{Axis: "X", Distance: 5000, Feed: 100},
		{Axis: "X", Distance: 1, Feed: 0},
	} {
		if err := bridge.Do(CommandJog, bad, 0); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
}

func TestTheAimingBeamIsHeldOnALease(t *testing.T) {
	// The aiming beam is the appliance's own hazard. With nobody connected, a
	// beam that is on is what TriggerBeamUnattended exists to stop - and it
	// would, within a second. The lease is what says somebody is there, and it
	// has to be renewed, so a closed laptop takes the beam with it.
	controller, _, bridge := startBridgeWith(t, func(c *Config) {
		c.IdlePoll = 100 * time.Millisecond
		c.BeamGrace = 300 * time.Millisecond
		c.OnDisconnect = DisconnectReset
	})
	sim := &controllerSim{t: t, controller: controller, state: "Idle", overrides: true, laserMode: "1"}
	done := make(chan struct{})
	go sim.run(done)
	t.Cleanup(func() { close(done) })
	waitFor(t, func() bool { return bridge.Status().Device != "" }, "the port to open")

	if err := bridge.Do(CommandAim, Jog{}, 2); err != nil {
		t.Fatal(err)
	}
	if !bridge.Aiming() {
		t.Fatal("the lease was not taken")
	}
	// Held: renewed the way the page does, and the watchdog leaves it alone
	// even though the beam is on with nobody connected.
	sim.set("Idle", true)
	for i := 0; i < 8; i++ {
		time.Sleep(200 * time.Millisecond)
		if err := bridge.Do(CommandAim, Jog{}, 2); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, resets := sim.counts(); resets != 0 {
		t.Fatalf("the watchdog reset a beam somebody was holding on purpose (%d resets)", resets)
	}

	// Let go without saying so - a browser tab that vanished - and it goes out.
	// Waiting for the record rather than for the lease: the lease reads as
	// expired the moment its deadline passes, and the beam is switched off on
	// the watchdog's next tick, which is later.
	waitFor(t, func() bool {
		for _, event := range bridge.Journal().Recent(0) {
			if strings.Contains(event.Text, "nobody renewed it") {
				return true
			}
		}
		return false
	}, "the beam to be released and the record to say so")
	if bridge.Aiming() {
		t.Error("the lease outlived its renewal")
	}
}

func TestSteeringNeedsAPort(t *testing.T) {
	bridge := New(Config{Device: "/dev/does-not-exist", Baudrate: 115200, Port: 0}, nil)
	if err := bridge.Do(CommandHome, Jog{}, 0); err != ErrNoPort {
		t.Fatalf("err = %v, want ErrNoPort", err)
	}
	_ = net.Dial // keep the import honest for the helpers above
}
