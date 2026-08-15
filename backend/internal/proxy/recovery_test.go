package proxy

import (
	"strings"
	"testing"
	"time"
)

// The invariant that ties the recovery paths together, written down as a test
// because it was previously only true by inspection.
//
// Four things restart this appliance, and they end in different places as far
// as the laser is concerned. supervise-daemon respawns a bridge that exited.
// BridgeWatch restarts one that is running but no longer answering. The
// hardware watchdog resets the board. GRUB's boot counter picks the other slot.
// Only the last two power-cycle USB, and only that re-enumeration toggles DTR
// and resets an Arduino-based controller - a bridge restart deliberately does
// not, because HUPCL is cleared so that restarting the service during a job
// costs nothing (ADR 0009).
//
// So a restarted bridge can find itself holding a port to a machine whose laser
// is on, having reset nothing. The invariant is: after any recovery, either the
// beam is off, or a client is in charge of it, or the appliance is watching it
// again within the grace period. These two tests are the first and second
// branches of that sentence.
func TestABridgeThatStartsOnALiveBeamStopsIt(t *testing.T) {
	controller, _, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectReset
		c.IdlePoll = 100 * time.Millisecond
		c.BeamGrace = 300 * time.Millisecond
	})
	// A machine standing still with the output on and nobody connected: what a
	// daemon that was just restarted mid-job can be handed.
	sim := &controllerSim{t: t, controller: controller, state: "Idle", beam: true, overrides: true, laserMode: "1"}
	done := make(chan struct{})
	go sim.run(done)
	t.Cleanup(func() { close(done) })

	// Neither a feed hold nor the spindle-stop override reaches a machine that
	// is already standing still, so the only rung that helps is the last.
	waitForCounts(t, sim, 0, 0, 1)
	sim.mu.Lock()
	beam := sim.beam
	sim.mu.Unlock()
	if beam {
		t.Error("a bridge started on a live beam left it burning")
	}
	var said bool
	for _, event := range bridge.Journal().Recent(0) {
		if strings.Contains(event.Text, "nobody connected") {
			said = true
		}
	}
	if !said {
		t.Error("it acted and the record does not say why")
	}
}

func TestABridgeDefersToAClientThatIsInCharge(t *testing.T) {
	// The other branch, and the one that makes the appliance usable at all:
	// aiming the beam by hand with LightBurn connected is a stationary burn on
	// purpose. With the stationary check switched off - which is what
	// stationary_beam_seconds=0 means, and what the appliance in the workshop
	// this was written for is running - nothing may interrupt it.
	sim, _, client := startWithSim(t, true, true, func(c *Config) {
		c.OnDisconnect = DisconnectReset
		c.IdlePoll = 100 * time.Millisecond
		c.BeamGrace = 200 * time.Millisecond
		c.StationaryBeam = 0
	})
	defer client.Close()
	sim.set("Idle", true)

	time.Sleep(time.Second)
	waitForCounts(t, sim, 0, 0, 0)
}
