package proxy

import (
	"strings"
	"testing"
	"time"
)

// A stationary beam is stopped on what the controller said about its outputs,
// and GRBL only mentions them alongside Ov: - every tenth report or so. By the
// time a twenty-second grace has run out, the oldest of that evidence is twenty
// seconds old, and in laser mode it was taken at an instant when the output
// happened to be on.
//
// That was tolerable while an intervention cost a feed hold. It is not now that
// the client is dropped with it, so the bridge no longer acts on a memory.
func TestAStationaryBeamIsNotStoppedOnEvidenceThatStoppedArriving(t *testing.T) {
	// overrides=false: the simulator answers without Ov:, so after the initial
	// reading nothing further is said about the outputs - exactly the shape of
	// a controller whose accessory field has simply not come round again.
	sim, bridge, client := startWithSim(t, true, true, func(c *Config) {
		c.OnDisconnect = DisconnectHold
		c.IdlePoll = 100 * time.Millisecond
		c.StationaryBeam = 400 * time.Millisecond
		c.BeamGrace = time.Hour
	})
	defer client.Close()
	// The beam was seen on; now the controller stops mentioning its outputs and
	// the machine stands still.
	waitFor(t, func() bool { return bridge.Status().Machine.Beam == "on" }, "the beam to be reported on")
	sim.silenceOverrides()
	sim.set("Idle", true)

	// Well past the grace period, and nothing may have been sent.
	time.Sleep(1500 * time.Millisecond)
	waitForCounts(t, sim, 0, 0, 0)

	// And it says so rather than staying quiet about a check it did not make.
	var said bool
	for _, event := range bridge.Journal().Recent(0) {
		if strings.Contains(event.Text, "said nothing about its outputs") {
			said = true
		}
	}
	if !said {
		t.Error("the bridge declined to act and left no record of why")
	}
}
