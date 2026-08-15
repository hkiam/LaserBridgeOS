package proxy

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// controllerSim answers status requests the way GRBL 1.1 does, and lets a test
// decide what it says about its own laser output.
//
// The point of these tests is not that the simulator behaves like a real
// controller - it behaves like my understanding of one, which is exactly the
// thing that cannot be verified here. What they check is that the bridge draws
// the right conclusion from each of the answers a controller can give.
type controllerSim struct {
	t          *testing.T
	controller interface {
		Read([]byte) (int, error)
		Write([]byte) (int, error)
	}

	mu sync.Mutex
	// state is what the machine reports; beam is whether it admits its output
	// is on; overrides decides whether it includes the Ov: field at all, which
	// is what makes an answer an answer.
	state     string
	beam      bool
	overrides bool
	// ignoreStop is a controller that does not implement the spindle-stop
	// override, or implements it and leaves the laser on anyway.
	ignoreStop bool
	holds      int
	stops      int
	resets     int
	pollCount  int
	x          float64
}

func (c *controllerSim) polls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pollCount
}

func (c *controllerSim) moveTo(x float64) {
	c.mu.Lock()
	c.x = x
	c.mu.Unlock()
}

// silenceOverrides makes the controller stop appending Ov: to its reports,
// which is what a real one does for nine reports out of ten.
func (c *controllerSim) silenceOverrides() {
	c.mu.Lock()
	c.overrides = false
	c.mu.Unlock()
}

func (c *controllerSim) refuseToStop() {
	c.mu.Lock()
	c.ignoreStop = true
	c.mu.Unlock()
}

func (c *controllerSim) set(state string, beam bool) {
	c.mu.Lock()
	c.state, c.beam = state, beam
	c.mu.Unlock()
}

func (c *controllerSim) counts() (holds, stops, resets int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.holds, c.stops, c.resets
}

func (c *controllerSim) run(done <-chan struct{}) {
	buffer := make([]byte, 256)
	for {
		select {
		case <-done:
			return
		default:
		}
		n, err := c.controller.Read(buffer)
		if err != nil {
			return
		}
		for _, b := range buffer[:n] {
			c.handle(b)
		}
	}
}

func (c *controllerSim) handle(b byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch b {
	case statusReq:
		c.pollCount++
		report := fmt.Sprintf("<%s|MPos:%.3f,2.000,0.000|FS:0,0", c.state, c.x)
		if c.overrides {
			report += "|Ov:100,100,100"
			if c.beam {
				report += "|A:S"
			}
		}
		_, _ = c.controller.Write([]byte(report + ">\r\n"))
	case feedHold:
		c.holds++
		// GRBL enters HOLD from a cycle or a jog. From Idle there is no motion
		// to suspend and the command is ignored - which is the whole reason
		// this simulator models the state machine rather than just setting the
		// state: getting this wrong made a green test out of a ladder whose
		// bottom two rungs do nothing.
		if c.state == "Run" || c.state == "Jog" {
			c.state = "Hold:0"
		}
	case spindleStop:
		c.stops++
		// The override acts only in HOLD. It is also a toggle; here it only
		// ever stops, because a test that let it start the laser would be
		// describing a bug rather than catching one.
		if c.state == "Hold:0" && !c.ignoreStop {
			c.beam = false
		}
	case softReset:
		c.resets++
		c.beam = false
		c.state = "Idle"
		_, _ = c.controller.Write([]byte("Grbl 1.1f ['$' for help]\r\n"))
	}
}

// startWithSim runs a bridge against a simulated controller and returns both,
// with a client already attached and the machine reported as cutting.
func startWithSim(t *testing.T, overrides, beam bool, adjust func(*Config)) (*controllerSim, *Bridge, net.Conn) {
	t.Helper()
	controller, address, bridge := startBridgeWith(t, func(c *Config) {
		c.HoldSettle = time.Second
		adjust(c)
	})
	sim := &controllerSim{t: t, controller: controller, state: "Run", beam: beam, overrides: overrides}
	done := make(chan struct{})
	go sim.run(done)
	t.Cleanup(func() { close(done) })

	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(bridge, StateClientConnected)
	// Ask once so the bridge knows the machine is cutting.
	if _, err := client.Write([]byte("?")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return bridge.Status().Machine.State == "Run" }, "the machine to report Run")
	return sim, bridge, client
}

// waitForCounts waits until the controller has actually received what the
// bridge decided to send, and then makes sure nothing further arrives.
//
// Reading the counters once, straight after waitForIntervention returns, is a
// race. The bridge records an intervention at the moment it decides on one; the
// simulator counts a command when the byte reaches the far end of a
// pseudo-terminal and its goroutine is scheduled to read it. Under load - the
// whole suite running at once - the second moment lands after the first, and
// the test then reports a safety step as missing when it was merely still in
// flight. It failed that way on a laptop and in CI, on a different test each
// time, which is the worst possible way for a test about switching a laser off
// to behave: nobody can tell a flake from a regression, so both get ignored.
//
// The settle at the end is the other half. Waiting for the expected numbers
// would pass the instant they are reached, and an escalation that arrives one
// rung too far - a soft reset after a spindle stop that already worked - would
// go unseen.
func waitForCounts(t *testing.T, sim *controllerSim, holds, stops, resets int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h, s, r := sim.counts()
		if h == holds && s == stops && r == resets {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("holds = %d, stops = %d, resets = %d; want %d, %d and %d", h, s, r, holds, stops, resets)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if h, s, r := sim.counts(); h != holds || s != stops || r != resets {
		t.Fatalf("kept going after the ladder was done: holds = %d, stops = %d, resets = %d; want %d, %d and %d",
			h, s, r, holds, stops, resets)
	}
}

func waitForIntervention(t *testing.T, bridge *Bridge, substring string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(bridge.Status().LastIntervention, substring) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Reporting what did happen, not just what did not: an intervention that
	// took the wrong branch is the interesting failure here.
	t.Fatalf("no intervention mentioning %q; the last one was %q", substring, bridge.Status().LastIntervention)
}

func TestHoldIsAcceptedWhenTheControllerSaysTheBeamIsOff(t *testing.T) {
	sim, bridge, client := startWithSim(t, true, false, func(c *Config) {
		c.OnDisconnect = DisconnectHold
	})
	client.Close()
	waitFor(t, func() bool { return bridge.handled.Load() > 0 }, "the departure to be handled")

	// One feed hold and nothing after it: the controller said the beam is off,
	// and ending the job anyway would make the setting a lie.
	waitForCounts(t, sim, 1, 0, 0)
}

func TestHoldEscalatesWhenTheBeamIsStillOn(t *testing.T) {
	// The case this exists for: a feed hold that stopped the axes and left the
	// laser burning a hole where it stands.
	sim, bridge, client := startWithSim(t, true, true, func(c *Config) {
		c.OnDisconnect = DisconnectHold
	})
	client.Close()
	waitForIntervention(t, bridge, "stopping the spindle output")

	// The spindle stop worked, so the job is left resumable rather than reset.
	waitForCounts(t, sim, 1, 1, 0)
}

func TestHoldResetsWhenTheBeamWillNotGoOff(t *testing.T) {
	sim, bridge, client := startWithSim(t, true, true, func(c *Config) {
		c.OnDisconnect = DisconnectHold
	})
	sim.refuseToStop()
	client.Close()

	waitForIntervention(t, bridge, "soft reset")
	// The full ladder, once each.
	waitForCounts(t, sim, 1, 1, 1)
	sim.mu.Lock()
	beam := sim.beam
	sim.mu.Unlock()
	if beam {
		t.Error("the beam was left on after the whole ladder")
	}
}

func TestHoldResetsWhenLaserModeIsOff(t *testing.T) {
	// With $32=0 the output is a spindle, and a feed hold deliberately leaves
	// a spindle running. That is not an open question, so the bridge does not
	// wait to be told twice - even though this controller never reports its
	// accessory state at all.
	sim, bridge, client := startWithSim(t, false, true, func(c *Config) {
		c.OnDisconnect = DisconnectHold
	})
	if _, err := client.Write([]byte("$$\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := sim.controller.Write([]byte("$32=0\r\nok\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return bridge.Status().Machine.LaserMode == "off" }, "laser mode to be read")

	client.Close()
	waitForIntervention(t, bridge, "laser mode")
	if _, _, resets := sim.counts(); resets == 0 {
		t.Error("a spindle-mode controller was left holding with the output on")
	}
}

func TestHoldRecordsWhenItCannotTell(t *testing.T) {
	// A controller that never includes Ov:, with laser mode unknown. Ending
	// the job on an absence of evidence is its own kind of unreliable, so the
	// bridge says what it does not know and leaves it there.
	sim, bridge, client := startWithSim(t, false, false, func(c *Config) {
		c.OnDisconnect = DisconnectHold
	})
	client.Close()
	waitForIntervention(t, bridge, "never reported whether the beam is off")

	if _, _, resets := sim.counts(); resets != 0 {
		t.Errorf("ended the job on an absence of evidence (%d resets)", resets)
	}
}

func TestResetAlwaysEndsWithTheOutputOff(t *testing.T) {
	// The default, and the reason it is the default: a soft reset switches the
	// output off whatever $32 says and whatever the controller reports.
	sim, bridge, client := startWithSim(t, false, true, func(c *Config) {
		c.OnDisconnect = DisconnectReset
	})
	client.Close()
	waitForIntervention(t, bridge, "soft reset")

	waitForCounts(t, sim, 1, 0, 1)
	sim.mu.Lock()
	beam := sim.beam
	sim.mu.Unlock()
	if beam {
		t.Error("the beam was left on")
	}
}

func TestSpindleStopIsNeverSentWithoutEvidence(t *testing.T) {
	// It is a toggle. Sent to a controller whose output is already stopped, it
	// switches the laser back on - so it may only ever follow a report that
	// says the beam is on.
	sim, bridge, client := startWithSim(t, true, false, func(c *Config) {
		c.OnDisconnect = DisconnectHold
	})
	client.Close()
	waitFor(t, func() bool { return bridge.handled.Load() > 0 }, "the departure to be handled")
	time.Sleep(300 * time.Millisecond)

	if _, stops, _ := sim.counts(); stops != 0 {
		t.Errorf("sent the spindle-stop toggle %d times with the beam reported off", stops)
	}
}

func TestWatchdogStopsAStationaryBeamWithTheClientStillConnected(t *testing.T) {
	// The case the disconnect handler cannot see: LightBurn hangs, its
	// connection stays up, the stream stops, and the machine stands still with
	// the laser on. Nothing about the client looks wrong.
	sim, bridge, client := startWithSim(t, true, true, func(c *Config) {
		c.OnDisconnect = DisconnectHold
		c.IdlePoll = 100 * time.Millisecond
		c.StationaryBeam = 400 * time.Millisecond
		c.BeamGrace = time.Hour // not this rule; the client is still attached
	})
	defer client.Close()
	sim.set("Idle", true)

	// A machine standing still cannot be reached by a feed hold or by the
	// spindle-stop override - GRBL ignores the first from Idle and the second
	// outside a hold - so the only rung that helps here is the last one.
	waitForIntervention(t, bridge, "soft reset")
	sim.mu.Lock()
	beam := sim.beam
	sim.mu.Unlock()
	if beam {
		t.Error("the laser is still on")
	}
}

func TestTakingTheMachineOverEndsTheClientsConnection(t *testing.T) {
	// Found on a real machine, and the reason this test exists at all: the
	// watchdog stopped a laser while LightBurn was connected, and LightBurn -
	// whose connection was never in question - carried on sending into a
	// controller that had just been reset. A soft reset empties GRBL's planner
	// and its receive buffer, so from that moment the two ends disagree about
	// where the job is, and what the machine executes is not what was sent.
	//
	// The test above proves the beam goes out. It never asked what the client
	// was told, which is how the hole survived.
	sim, _, client := startWithSim(t, true, true, func(c *Config) {
		c.OnDisconnect = DisconnectHold
		c.IdlePoll = 100 * time.Millisecond
		c.StationaryBeam = 400 * time.Millisecond
		c.BeamGrace = time.Hour // not this rule; the client is still attached
	})
	defer client.Close()
	sim.set("Idle", true)

	// Everything the bridge says to the client, until it stops saying anything.
	if err := client.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var heard strings.Builder
	buffer := make([]byte, 512)
	var readErr error
	for {
		n, err := client.Read(buffer)
		heard.Write(buffer[:n])
		if err != nil {
			readErr = err
			break
		}
	}
	// The connection ending is what actually stops a sender; a message it can
	// ignore is not enough on its own.
	if errors.Is(readErr, os.ErrDeadlineExceeded) {
		t.Fatalf("the client was left connected to a machine the bridge had taken over: %q", heard.String())
	}
	// And it is told why, in GRBL's own way of speaking to a sender - which
	// LightBurn prints in its console, and which cannot be counted as an "ok".
	if !strings.Contains(heard.String(), "[MSG:LaserBridgeOS took over") {
		t.Errorf("the client was dropped without being told why: %q", heard.String())
	}

	// Exactly one reset. The disconnect the bridge caused itself must not start
	// a second ladder into a controller that was just reset.
	waitForCounts(t, sim, 0, 0, 1)
}

func TestAStandingMachineIsResetRatherThanCoaxed(t *testing.T) {
	// The rung that does nothing is worse than no rung: a feed hold from Idle
	// and a spindle-stop outside a hold are both ignored by GRBL, so trying
	// them first only means another second of beam.
	sim, bridge, client := startWithSim(t, true, true, func(c *Config) {
		c.OnDisconnect = DisconnectHold
	})
	// The bridge has to have seen the machine standing still; changing the
	// controller without letting anyone ask about it proves nothing.
	sim.set("Idle", true)
	if _, err := client.Write([]byte("?")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return bridge.Status().Machine.State == "Idle" }, "the machine to report Idle")
	client.Close()

	waitForIntervention(t, bridge, "nothing gentler reaches it")
	// Neither a feed hold nor the override reaches a machine standing still, so
	// only the reset is sent.
	waitForCounts(t, sim, 0, 0, 1)
}

func TestWatchdogLeavesAMovingMachineAlone(t *testing.T) {
	// A cutting machine has the laser on and that is the entire point. The
	// check is about standing still, and a watchdog that could not tell the
	// difference would be worse than none.
	sim, _, client := startWithSim(t, true, true, func(c *Config) {
		c.OnDisconnect = DisconnectHold
		c.IdlePoll = 100 * time.Millisecond
		c.StationaryBeam = 400 * time.Millisecond
		c.BeamGrace = time.Hour
	})
	defer client.Close()

	moving := make(chan struct{})
	go func() {
		defer close(moving)
		for i := 0; i < 40; i++ {
			sim.mu.Lock()
			sim.state = "Run"
			sim.mu.Unlock()
			sim.moveTo(float64(i))
			time.Sleep(50 * time.Millisecond)
		}
	}()
	<-moving

	if holds, stops, resets := sim.counts(); holds+stops+resets != 0 {
		t.Errorf("interrupted a cutting machine: %d holds, %d stops, %d resets", holds, stops, resets)
	}
}

func TestWatchdogRespectsBeingToldToKeepOut(t *testing.T) {
	// on_disconnect=none is a promise that the bridge never commands the
	// machine. A watchdog that acted anyway would make the setting a lie.
	sim, _, client := startWithSim(t, true, true, func(c *Config) {
		c.OnDisconnect = DisconnectNone
		c.IdlePoll = 100 * time.Millisecond
		c.StationaryBeam = 300 * time.Millisecond
		c.BeamGrace = 300 * time.Millisecond
	})
	client.Close()
	sim.set("Idle", true)
	time.Sleep(1500 * time.Millisecond)

	if holds, stops, resets := sim.counts(); holds+stops+resets != 0 {
		t.Errorf("acted despite being told not to: %d holds, %d stops, %d resets", holds, stops, resets)
	}
}

func TestWatchdogAsksOnlyWhenNobodyElseIs(t *testing.T) {
	// The one thing the appliance injects into a client's stream, and only
	// when the conversation has stopped. A client that is polling should never
	// see a status request it did not send.
	sim, _, client := startWithSim(t, true, false, func(c *Config) {
		c.OnDisconnect = DisconnectHold
		c.IdlePoll = 400 * time.Millisecond
		c.StationaryBeam = time.Hour
		c.BeamGrace = time.Hour
	})
	defer client.Close()

	before := sim.polls()
	// The client keeps asking, faster than the appliance's own interval.
	for i := 0; i < 12; i++ {
		if _, err := client.Write([]byte("?")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := sim.polls() - before; got > 13 {
		t.Errorf("the controller was asked %d times for 12 client requests; the appliance chimed in", got)
	}
	// And when the client stops, the appliance takes over the asking.
	quiet := sim.polls()
	time.Sleep(1500 * time.Millisecond)
	if sim.polls() <= quiet {
		t.Error("nobody asked the controller anything once the client went quiet")
	}
}

func TestNoSecondHoldOnAMachineAlreadyMadeSafe(t *testing.T) {
	// The watchdog gets there first and leaves the machine standing still with
	// the output off. When the client's departure is then noticed, there is
	// nothing left to do - and doing it anyway would put a line in the record
	// implying something was wrong.
	sim, bridge, client := startWithSim(t, true, true, func(c *Config) {
		c.OnDisconnect = DisconnectHold
		c.IdlePoll = 100 * time.Millisecond
		c.StationaryBeam = 300 * time.Millisecond
		c.BeamGrace = time.Hour
	})
	sim.set("Idle", true)
	// A standing machine can only be reached by the last rung.
	waitForIntervention(t, bridge, "soft reset")
	holdsBefore, _, _ := sim.counts()

	client.Close()
	waitFor(t, func() bool { return bridge.handled.Load() > 0 }, "the departure to be handled")
	time.Sleep(300 * time.Millisecond)

	if holds, _, _ := sim.counts(); holds != holdsBefore {
		t.Errorf("held a machine that was already standing still with the beam off (%d then %d)", holdsBefore, holds)
	}
}

func TestTheWatchdogKeepsWatchingWhileSomethingElseIsBusy(t *testing.T) {
	// The watchdog and the disconnect handler can reach for the machine at the
	// same moment, and only one may have it. What the waiting one must not do
	// is forget how long the beam has been on: restarting the grace period on
	// every blocked look means that after a long intervention the beam gets a
	// fresh grace period before anything examines it again - exactly when it
	// has already been on too long.
	//
	// So the measure is how quickly it acts once the lock frees, not whether
	// it acts at all. Long grace, longer hold: with the timers kept, the very
	// next look acts; with them reset, it waits another two seconds first.
	const grace = 2 * time.Second
	sim, bridge, client := startWithSim(t, true, true, func(c *Config) {
		c.OnDisconnect = DisconnectHold
		c.IdlePoll = 100 * time.Millisecond
		c.StationaryBeam = grace
		c.BeamGrace = grace
		c.HoldSettle = 200 * time.Millisecond
	})
	defer client.Close()
	sim.set("Idle", true)

	bridge.interveneMu.Lock()
	time.Sleep(grace + time.Second)
	if _, _, resets := sim.counts(); resets != 0 {
		t.Fatalf("something acted while the lock was held (%d resets)", resets)
	}

	released := time.Now()
	bridge.interveneMu.Unlock()
	waitForIntervention(t, bridge, "soft reset")
	// With the timers kept this is one tick plus the ladder; with them reset
	// it is a whole grace period. Half of one separates the two comfortably.
	if waited := time.Since(released); waited > grace/2 {
		t.Errorf("acted %s after the lock freed; the beam had already been on for %s and the "+
			"wait suggests the clock was restarted", waited.Round(time.Millisecond), grace)
	}
}
