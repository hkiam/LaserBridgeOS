package proxy

import (
	"net"
	"testing"
	"time"
)

// The case the watchdog exists for is a sender that has stopped reading. Its
// receive window fills, and the message explaining why it is about to be
// dropped is then the one write that blocks. With the ordinary five-second
// patience that would delay the feed hold by five seconds, to be polite to a
// program that is not listening.
func TestAClientThatStoppedReadingDoesNotDelayTheStop(t *testing.T) {
	sim, _, client := startWithSim(t, true, true, func(c *Config) {
		c.OnDisconnect = DisconnectHold
		c.IdlePoll = 100 * time.Millisecond
		c.StationaryBeam = 300 * time.Millisecond
		c.BeamGrace = time.Hour
		c.ClientWriteTimeout = 30 * time.Second // the patience a job deserves
	})
	defer client.Close()
	// Stop reading and let the socket fill: the bridge forwards every status
	// report to this connection and nothing is draining it.
	if tcp, ok := client.(*net.TCPConn); ok {
		if err := tcp.SetReadBuffer(1024); err != nil {
			t.Fatal(err)
		}
	}
	sim.set("Idle", true)

	// The machine has to be stopped regardless of what the client is doing.
	// Generous enough not to be flaky, far below the write timeout above.
	deadline := time.Now().Add(6 * time.Second)
	for {
		if _, _, resets := sim.counts(); resets > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the laser was left on while the bridge waited for a client that was not reading")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
