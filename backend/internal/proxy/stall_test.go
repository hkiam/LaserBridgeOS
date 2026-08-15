package proxy

import (
	"net"
	"testing"
	"time"
)

func TestAStalledClientDoesNotStallTheSerialPort(t *testing.T) {
	// A laptop on a dead Wi-Fi link is still connected as far as the kernel is
	// concerned; its receive window simply never opens. Reproducing that with
	// a real socket would mean filling hundreds of kilobytes of kernel buffer,
	// so the stall is staged with net.Pipe - a genuine net.Conn that blocks on
	// write until someone reads, and that honours deadlines.
	//
	// Without a write deadline the bridge parks in that write forever, and
	// with it the only goroutine reading the serial port: the controller talks
	// into a kernel buffer nobody drains and its output is lost. Nothing else
	// may rescue it - no second client arrives here, because a test that let
	// one arrive would pass either way.
	controller, _, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectNone
		c.ClientWriteTimeout = 300 * time.Millisecond
	})

	stalled, nobody := net.Pipe()
	defer stalled.Close()
	defer nobody.Close()
	bridge.clientMu.Lock()
	bridge.client = stalled
	bridge.clientMu.Unlock()

	// The first report is the one the bridge will block on delivering.
	if _, err := controller.Write([]byte("<Idle|MPos:0.000,0.000,0.000>\r\n")); err != nil {
		t.Fatal(err)
	}
	// The second sits in the serial buffer until somebody reads it, which only
	// happens if the bridge gave up on the client.
	if _, err := controller.Write([]byte("<Run|MPos:4.000,0.000,0.000|FS:600,255>\r\n")); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return bridge.Status().Machine.State == "Run" },
		"the serial port to be read again after the client stopped accepting data")
}
