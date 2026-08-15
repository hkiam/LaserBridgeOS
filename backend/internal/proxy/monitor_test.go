package proxy

import (
	"bufio"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func dialMonitor(t *testing.T, port int) *bufio.Reader {
	t.Helper()
	var conn net.Conn
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err = net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("monitor port never opened: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	reader := bufio.NewReader(conn)
	// The greeting, which also proves the connection is live.
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	return reader
}

func TestMonitorShowsBothDirections(t *testing.T) {
	monitorPort := freePort(t)
	controller, address, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectNone
		c.MonitorPort = monitorPort
	})
	watcher := dialMonitor(t, monitorPort)
	waitFor(t, func() bool { return bridge.Status().Monitors == 1 }, "the monitor to be counted")

	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	waitForState(bridge, StateClientConnected)

	if _, err := client.Write([]byte("G0 X1\n")); err != nil {
		t.Fatal(err)
	}
	mustRead(t, controller, 6)
	if _, err := controller.Write([]byte("ok\r\n")); err != nil {
		t.Fatal(err)
	}
	mustRead(t, client, 4)

	// Both directions reach the watcher, each marked with who said it, and
	// each on its own line - chunk boundaries from the network and the serial
	// port have nothing to do with the conversation.
	transcript := readTranscript(t, watcher, 2)
	if !strings.Contains(transcript, "> G0 X1\n") {
		t.Errorf("the outgoing line is missing or not a line: %q", transcript)
	}
	if !strings.Contains(transcript, "< ok\n") {
		t.Errorf("the reply is missing or not a line: %q", transcript)
	}
}

func readTranscript(t *testing.T, watcher *bufio.Reader, lines int) string {
	t.Helper()
	transcript := ""
	for i := 0; i < lines; i++ {
		line, err := watcher.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the transcript: %v (so far: %q)", err, transcript)
		}
		transcript += line
	}
	return transcript
}

func TestMonitorAssemblesLinesAndNamesRealtimeBytes(t *testing.T) {
	monitorPort := freePort(t)
	controller, address, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectNone
		c.MonitorPort = monitorPort
	})
	watcher := dialMonitor(t, monitorPort)
	waitFor(t, func() bool { return bridge.Status().Monitors == 1 }, "the monitor to attach")

	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	waitForState(bridge, StateClientConnected)

	// A status request carries no line ending and must not sit in a buffer
	// waiting for one; it is a command in its own right.
	if _, err := client.Write([]byte("?")); err != nil {
		t.Fatal(err)
	}
	mustRead(t, controller, 1)
	// A reply split across two reads, the way a serial port really delivers.
	if _, err := controller.Write([]byte("<Idle|MPos:0.000,0.0")); err != nil {
		t.Fatal(err)
	}
	mustRead(t, client, 20)
	if _, err := controller.Write([]byte("00,0.000|FS:0,0>\r\n")); err != nil {
		t.Fatal(err)
	}
	mustRead(t, client, 18)

	transcript := readTranscript(t, watcher, 2)
	if !strings.Contains(transcript, "> ? (status)\n") {
		t.Errorf("the status request was not named: %q", transcript)
	}
	// One line, reassembled, not two halves.
	if !strings.Contains(transcript, "< <Idle|MPos:0.000,0.000,0.000|FS:0,0>\n") {
		t.Errorf("the reply was not reassembled: %q", transcript)
	}
}

func TestMonitorCannotSteerTheMachine(t *testing.T) {
	// The whole point of a read-only port: watching a job must not be able to
	// become part of it.
	monitorPort := freePort(t)
	controller, address, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectNone
		c.MonitorPort = monitorPort
	})

	// The monitor listener comes up on its own goroutine, so give it a moment
	// rather than racing it.
	var conn net.Conn
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err = net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(monitorPort))); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("monitor port never opened: %v", err)
	}
	defer conn.Close()
	waitFor(t, func() bool { return bridge.Status().Monitors == 1 }, "the monitor to attach")

	// A soft reset from a watcher must go nowhere.
	if _, err := conn.Write([]byte{softReset, 'M', '3', '\n'}); err != nil {
		t.Fatal(err)
	}

	// Proved by what the controller receives next: the real client's byte, and
	// nothing before it.
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	waitForState(bridge, StateClientConnected)
	if _, err := client.Write([]byte("Z")); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, controller, 1); got[0] != 'Z' {
		t.Fatalf("the controller received %q from a read-only watcher", got)
	}
}

func TestMonitorIsOffUnlessAskedFor(t *testing.T) {
	_, _, bridge := startBridge(t, true)
	if bridge.Status().Monitors != 0 {
		t.Error("monitors were counted with no monitor port configured")
	}
}

func TestMonitorMarksTheBridgesOwnCommands(t *testing.T) {
	// The transcript's most important line is the one nobody sent: the feed
	// hold the appliance decided on. Showing it as if the client had sent it
	// would mislead about exactly the thing worth watching for.
	monitorPort := freePort(t)
	controller, address, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectHold
		c.MonitorPort = monitorPort
	})
	watcher := dialMonitor(t, monitorPort)
	waitFor(t, func() bool { return bridge.Status().Monitors == 1 }, "the monitor to attach")

	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(bridge, StateClientConnected)
	tellMachine(t, controller, bridge, "<Run|MPos:1.000,1.000,0.000|FS:600,255>")
	client.Close()

	transcript := ""
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(transcript, "*") && time.Now().Before(deadline) {
		line, err := watcher.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the transcript: %v (so far: %q)", err, transcript)
		}
		transcript += line
	}
	if !strings.Contains(transcript, "* ! (feed hold)\n") {
		t.Errorf("the bridge's own feed hold is not marked as its own: %q", transcript)
	}
}
