package proxy

import (
	"strings"
	"testing"
	"time"
)

// The guard that refuses to restart the bridge or install an update "while the
// machine is moving" was false during a pierce, during a pause, and while
// somebody changed the material - so it was open at exactly the moments it
// exists for. A job is the span the operator means, and it survives the machine
// standing still for a moment.
func TestAJobSurvivesTheMachineStandingStill(t *testing.T) {
	sim, bridge, client := startWithSim(t, true, false, func(c *Config) {
		c.OnDisconnect = DisconnectNone // nothing here is about intervening
		c.IdlePoll = 100 * time.Millisecond
	})
	defer client.Close()

	waitFor(t, func() bool { return bridge.Status().Job.Running }, "the job to begin")
	started := bridge.Status().Job.StartedUnix
	if started == 0 {
		t.Fatal("a job began without a time")
	}

	// A pierce: the machine stops dead for longer than any "is it moving now"
	// question would tolerate, and the job is still the same job.
	sim.set("Idle", false)
	time.Sleep(time.Second)
	job := bridge.Status().Job
	if !job.Running {
		t.Fatal("a pause ended the job; the guard would have let an update through")
	}
	if job.StartedUnix != started {
		t.Error("the pause started a new job rather than continuing the old one")
	}

	// And the lines the client sent during it are counted, because that is what
	// a sender counts too.
	if _, err := client.Write([]byte("G1X10F800\nG1X20\nG1X30\n")); err != nil {
		t.Fatal(err)
	}
	sim.set("Run", false)
	waitFor(t, func() bool { return bridge.Status().Job.Lines >= 3 }, "the lines to be counted")
}

func TestAJobEndsWhenTheWorkIsOverAndSaysSo(t *testing.T) {
	sim, bridge, client := startWithSim(t, true, false, func(c *Config) {
		c.OnDisconnect = DisconnectNone
		c.IdlePoll = 100 * time.Millisecond
	})
	waitFor(t, func() bool { return bridge.Status().Job.Running }, "the job to begin")

	// The client leaving ends it at once: whatever the machine does with what
	// is left in its buffer, the work in front of it is over.
	client.Close()
	waitFor(t, func() bool { return !bridge.Status().Job.Running }, "the job to end")

	job := bridge.Status().Job
	if job.Ended != "ended with the client" {
		t.Errorf("ended = %q, want it to say why", job.Ended)
	}
	var recorded bool
	for _, event := range bridge.Journal().Recent(0) {
		if strings.HasPrefix(event.Text, "job ended with the client") {
			recorded = true
		}
	}
	if !recorded {
		t.Error("a job ended and the record does not mention it")
	}
	_ = sim
}
