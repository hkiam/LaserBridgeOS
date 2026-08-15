package proxy

import (
	"strings"
	"testing"
	"time"
)

// A stationary beam is stopped on what the controller said about its outputs,
// and GRBL only mentions them alongside Ov: - every tenth report when idle,
// every twentieth while moving. By the time a grace period has run out, the
// oldest of that evidence can be the whole grace period old, and in laser mode
// it was taken at an instant when the output happened to be on.
//
// That was tolerable while an intervention cost a feed hold. It is not now that
// the client is dropped with it, so the bridge no longer acts on a memory: the
// controller has to have mentioned its outputs within the last thirty reports.
func TestAStationaryBeamIsNotStoppedOnEvidenceThatStoppedArriving(t *testing.T) {
	sim, bridge, client := startWithSim(t, true, true, func(c *Config) {
		c.OnDisconnect = DisconnectHold
		c.IdlePoll = 100 * time.Millisecond
		c.StationaryBeam = time.Second
		c.BeamGrace = time.Hour
	})
	defer client.Close()

	// A client polling the way one does during a job. It matters that reports
	// arrive quickly: the tolerance is thirty reports, and how long thirty
	// reports take is set by whoever is asking. Left to its own idle polling
	// the appliance manages about six a second, so the same thirty reports
	// would take five seconds - which is a fact about this check worth knowing
	// and the reason the number is counted in reports rather than in seconds.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		buffer := make([]byte, 4096)
		for {
			if _, err := client.Read(buffer); err != nil {
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := client.Write([]byte("?")); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// The beam was seen on; now the controller stops mentioning its outputs and
	// the machine stands still.
	waitFor(t, func() bool { return bridge.Status().Machine.Beam == "on" }, "the beam to be reported on")
	sim.silenceOverrides()
	sim.set("Idle", true)
	waitFor(t, func() bool {
		machine := bridge.Status().Machine
		return machine.Reports-machine.BeamReports > beamEvidenceReports
	}, "the evidence about the outputs to go stale")

	// Well past the grace period, and nothing may have been sent.
	time.Sleep(2 * time.Second)
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
