package proxy

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
)

// startBridge runs a bridge against a pseudo-terminal and returns the
// controller side of it, the address to connect to, and a stop function.
func startBridge(t *testing.T, kickOldClient bool) (controller *os.File, address string, bridge *Bridge) {
	t.Helper()
	// The default for the older tests: the bridge carries bytes and does
	// nothing on its own, which is what they were written to check.
	return startBridgeWith(t, func(c *Config) {
		c.KickOldClient = kickOldClient
		c.OnDisconnect = DisconnectNone
	})
}

// startBridgeWith is the same, with the configuration open to adjustment.
func startBridgeWith(t *testing.T, adjust func(*Config)) (controller *os.File, address string, bridge *Bridge) {
	t.Helper()
	controller, devicePath, err := openPTY()
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	// Hand the port straight back; the bridge opens its own listener and the
	// window in between is not worth a more elaborate arrangement in a test.
	listener.Close()

	settings := Config{
		Device:      devicePath,
		Baudrate:    115200,
		Port:        port,
		DeviceRetry: 20 * time.Millisecond,
		HoldSettle:  500 * time.Millisecond,
		// The standing supervision is off unless a test asks for it. It polls
		// the controller whenever no client is attached, and a test reading
		// raw bytes to check that a client's traffic crosses unchanged would
		// otherwise find the appliance's own status requests mixed into them.
		IdlePoll:  time.Hour,
		BeamGrace: time.Hour,
	}
	adjust(&settings)
	bridge = New(settings, nil)

	done := make(chan struct{})
	ready := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if err := bridge.Run(done, ready); err != nil {
			t.Errorf("bridge stopped: %v", err)
		}
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not start listening")
	}

	t.Cleanup(func() {
		close(done)
		<-finished
		// Closing twice is harmless and lets a test close it early to
		// simulate the adapter being unplugged.
		_ = controller.Close()
	})
	return controller, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), bridge
}

// mustRead reads exactly count bytes, tolerating the one way a
// pseudo-terminal differs from a serial port: when the bridge closes the
// device side between clients, the controller side reports EIO once. A real
// adapter simply stays open, so this is the harness absorbing an artefact
// rather than the bridge misbehaving.
func mustRead(t *testing.T, reader io.Reader, count int) []byte {
	t.Helper()
	buffer := make([]byte, count)
	filled := 0
	deadline := time.Now().Add(10 * time.Second)
	for filled < count {
		n, err := reader.Read(buffer[filled:])
		filled += n
		if err == nil || filled == count {
			continue
		}
		if errors.Is(err, syscall.EIO) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		t.Fatalf("reading %d bytes (got %d): %v", count, filled, err)
	}
	return buffer
}

func TestBytesCrossUnchangedInBothDirections(t *testing.T) {
	controller, address, _ := startBridge(t, true)

	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// G-code towards the controller.
	gcode := []byte("G0 X10 Y10\nG1 F600\n")
	if _, err := client.Write(gcode); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, controller, len(gcode)); !bytes.Equal(got, gcode) {
		t.Fatalf("controller saw %q, want %q", got, gcode)
	}

	// Replies back to the client.
	reply := []byte("Grbl 1.1h ['$' for help]\r\nok\r\n")
	if _, err := controller.Write(reply); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, client, len(reply)); !bytes.Equal(got, reply) {
		t.Fatalf("client saw %q, want %q", got, reply)
	}
}

func TestControlBytesArriveIntact(t *testing.T) {
	controller, address, _ := startBridge(t, true)
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// The bytes that matter most: soft reset, feed hold, resume, status
	// request. A bridge that translates, buffers by line, or swallows any of
	// these is worse than no bridge - 0x18 is how a laser is stopped.
	control := []byte{0x18, '!', '~', '?', 0x00, 0xff, '\r', '\n'}
	if _, err := client.Write(control); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, controller, len(control)); !bytes.Equal(got, control) {
		t.Fatalf("controller saw % x, want % x", got, control)
	}

	if _, err := controller.Write(control); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, client, len(control)); !bytes.Equal(got, control) {
		t.Fatalf("client saw % x, want % x", got, control)
	}
}

func TestDisconnectIsNoticedAndPortReleased(t *testing.T) {
	controller, address, bridge := startBridge(t, true)

	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("?")); err != nil {
		t.Fatal(err)
	}
	mustRead(t, controller, 1)

	if state := waitForState(bridge, StateClientConnected); state != StateClientConnected {
		t.Fatalf("state = %s, want CLIENT_CONNECTED", state)
	}
	client.Close()

	if state := waitForState(bridge, StateListening); state != StateListening {
		t.Fatalf("state after disconnect = %s, want LISTENING", state)
	}

	// The port must be free for the next client, which is the whole point of
	// noticing the disconnect.
	second, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("reconnect refused: %v", err)
	}
	defer second.Close()
	if _, err := second.Write([]byte("$$")); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, controller, 2); string(got) != "$$" {
		t.Fatalf("after reconnect controller saw %q", got)
	}
}

func TestSecondClientTakesOverWhenKickingIsOn(t *testing.T) {
	controller, address, bridge := startBridge(t, true)

	first, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := first.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	mustRead(t, controller, 1)
	waitForState(bridge, StateClientConnected)

	second, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.Write([]byte("b")); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, controller, 1); string(got) != "b" {
		t.Fatalf("controller saw %q from the second client", got)
	}

	// The first client must be gone, not silently ignored: two applications
	// believing they control the laser is the situation to avoid.
	_ = first.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := first.Read(make([]byte, 1)); err == nil {
		t.Fatal("the first client was left connected")
	}
}

func TestSecondClientIsRefusedWhenKickingIsOff(t *testing.T) {
	controller, address, bridge := startBridge(t, false)

	first, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := first.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	mustRead(t, controller, 1)
	waitForState(bridge, StateClientConnected)

	second, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_ = second.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := second.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("second client was not refused: %v", err)
	}

	// The first client keeps the port.
	if _, err := first.Write([]byte("c")); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, controller, 1); string(got) != "c" {
		t.Fatalf("first client lost the port, controller saw %q", got)
	}
	if bridge.Status().ClientsRejected != 1 {
		t.Fatalf("rejections = %d, want 1", bridge.Status().ClientsRejected)
	}
}

func TestStatusSocketReportsTheBridge(t *testing.T) {
	controller, address, bridge := startBridge(t, true)

	socketPath := filepath.Join(t.TempDir(), "laserbridged.sock")
	socket := NewStatusSocket(socketPath, bridge)
	done := make(chan struct{})
	go func() {
		if err := socket.Serve(done); err != nil {
			t.Errorf("status socket: %v", err)
		}
	}()
	t.Cleanup(func() { close(done) })

	waitFor(t, func() bool {
		_, err := os.Stat(socketPath)
		return err == nil
	}, "status socket to appear")

	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	payload := []byte("G0 X1\n")
	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	mustRead(t, controller, len(payload))
	waitForState(bridge, StateClientConnected)

	status, err := ReadStatus(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateClientConnected {
		t.Fatalf("state = %s", status.State)
	}
	if status.Client == "" || status.ConnectedSince == 0 {
		t.Fatalf("client details missing: %+v", status)
	}
	if status.RxBytes != uint64(len(payload)) {
		t.Fatalf("rx = %d, want %d", status.RxBytes, len(payload))
	}
}

func waitForState(bridge *Bridge, want State) State {
	deadline := time.Now().Add(5 * time.Second)
	var state State
	for time.Now().Before(deadline) {
		state = bridge.Status().State
		if state == want {
			return state
		}
		time.Sleep(10 * time.Millisecond)
	}
	return state
}

func waitFor(t *testing.T, condition func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestClientIsDroppedWhenTheDeviceDisappears(t *testing.T) {
	controller, address, bridge := startBridge(t, true)

	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("?")); err != nil {
		t.Fatal(err)
	}
	mustRead(t, controller, 1)
	waitForState(bridge, StateClientConnected)

	// Closing the controller side is what an unplugged USB adapter looks
	// like from the bridge: reads fail and the device is simply gone.
	controller.Close()

	// The client must not be left believing it still has a laser.
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("client survived the device disappearing")
	}
	if state := waitForState(bridge, StateWaitingForDevice); state != StateWaitingForDevice {
		t.Fatalf("state = %s, want WAITING_FOR_DEVICE", state)
	}
}

func TestStateMovesOnEvenWithoutADevice(t *testing.T) {
	// A bridge whose adapter never appears still accepts clients; when one
	// leaves, the state must say so rather than stay on CLIENT_CONNECTED
	// just because there is no port to fall back to.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	bridge := New(Config{
		Device:      filepath.Join(t.TempDir(), "absent"),
		Baudrate:    115200,
		Port:        port,
		DeviceRetry: 20 * time.Millisecond,
	}, nil)
	done := make(chan struct{})
	ready := make(chan struct{})
	finished := make(chan struct{})
	go func() { defer close(finished); _ = bridge.Run(done, ready) }()
	<-ready
	t.Cleanup(func() { close(done); <-finished })

	if state := waitForState(bridge, StateWaitingForDevice); state != StateWaitingForDevice {
		t.Fatalf("state = %s, want WAITING_FOR_DEVICE", state)
	}
	client, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	if state := waitForState(bridge, StateClientConnected); state != StateClientConnected {
		t.Fatalf("state = %s, want CLIENT_CONNECTED", state)
	}
	if bridge.Status().Client == "" {
		t.Fatal("status does not name the connected client")
	}
	client.Close()
	if state := waitForState(bridge, StateWaitingForDevice); state != StateWaitingForDevice {
		t.Fatalf("state after disconnect = %s, want WAITING_FOR_DEVICE", state)
	}
}

// tellMachine has the controller side announce a state, the way GRBL answers a
// status request, and waits until the bridge has taken it in.
func tellMachine(t *testing.T, controller *os.File, bridge *Bridge, line string) {
	t.Helper()
	if _, err := controller.Write([]byte(line + "\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return bridge.Status().Machine.LastReportUnix != 0 }, "the bridge to read the controller's report")
}

func TestReadsAlongWithoutAlteringTheStream(t *testing.T) {
	controller, address, bridge := startBridge(t, true)
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	waitForState(bridge, StateClientConnected)

	report := "<Run|MPos:12.500,3.000,0.000|FS:900,255>\r\n"
	if _, err := controller.Write([]byte(report)); err != nil {
		t.Fatal(err)
	}
	// The client must receive exactly what the controller sent - reading along
	// happens on a copy.
	if got := string(mustRead(t, client, len(report))); got != report {
		t.Fatalf("client received %q, want %q", got, report)
	}
	waitFor(t, func() bool { return bridge.Status().Machine.State == "Run" }, "the machine reading to catch up")

	machine := bridge.Status().Machine
	if machine.Position.X != 12.5 || machine.Spindle != 255 {
		t.Errorf("machine = %+v", machine)
	}
}

func TestFeedHoldWhenClientVanishesMidJob(t *testing.T) {
	controller, address, bridge := startBridgeWith(t, func(c *Config) {
		c.KickOldClient = true
		c.OnDisconnect = DisconnectHold
	})
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(bridge, StateClientConnected)
	tellMachine(t, controller, bridge, "<Run|MPos:1.000,1.000,0.000|FS:600,255>")

	// The laptop goes to sleep mid-job.
	client.Close()

	if got := mustRead(t, controller, 1); got[0] != feedHold {
		t.Fatalf("controller received %q, want a feed hold", got)
	}
	waitFor(t, func() bool { return bridge.Status().LastIntervention != "" }, "the intervention to be recorded")
}

func TestNoHoldWhenMachineIsIdle(t *testing.T) {
	controller, address, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectHold
	})
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(bridge, StateClientConnected)
	tellMachine(t, controller, bridge, "<Idle|MPos:0.000,0.000,0.000|FS:0,0>")

	client.Close()

	// Nothing was moving, so nothing should have been sent. A hold here would
	// leave the next client a paused machine to clear for no reason.
	//
	// Proved by what the controller sees next rather than by waiting for
	// silence: the next client's first byte must be the first byte to arrive.
	expectNothingWasInjected(t, controller, bridge, address)
	if bridge.Status().LastIntervention != "" {
		t.Errorf("intervention recorded for an idle machine: %q", bridge.Status().LastIntervention)
	}
}

// expectNothingWasInjected checks that the bridge sent nothing of its own
// after a client left, by having the next client send a byte that could not be
// mistaken for a GRBL control character and requiring it to arrive first.
func expectNothingWasInjected(t *testing.T, controller *os.File, bridge *Bridge, address string) {
	t.Helper()
	waitFor(t, func() bool { return bridge.handled.Load() > 0 }, "the departure to be handled")

	next, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if _, err := next.Write([]byte("Z")); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, controller, 1); got[0] != 'Z' {
		t.Fatalf("controller received %q before the next client's byte", got)
	}
}

func TestResetWaitsForTheMachineToStop(t *testing.T) {
	controller, address, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectReset
		c.HoldSettle = 3 * time.Second
	})
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(bridge, StateClientConnected)
	tellMachine(t, controller, bridge, "<Run|MPos:1.000,1.000,0.000|FS:600,255>")
	client.Close()

	// First the hold.
	if got := mustRead(t, controller, 1); got[0] != feedHold {
		t.Fatalf("first byte = %q, want a feed hold", got)
	}
	// Then the bridge asks whether the machine has stopped. A soft reset
	// during deceleration loses the position, so it must not arrive yet.
	if got := mustRead(t, controller, 1); got[0] != statusReq {
		t.Fatalf("second byte = %q, want a status request", got)
	}
	// The machine is still decelerating, then comes to rest.
	if _, err := controller.Write([]byte("<Run|MPos:1.100,1.000,0.000|FS:200,255>\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return bridge.Status().Machine.Feed == 200 }, "the deceleration report to be read")
	if _, err := controller.Write([]byte("<Hold:0|MPos:1.200,1.000,0.000|FS:0,0>\r\n")); err != nil {
		t.Fatal(err)
	}

	// Only now the reset. Status requests and the settings probe may arrive in
	// between; neither commands anything.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := mustRead(t, controller, 1)
		if got[0] == softReset {
			break
		}
		if got[0] != statusReq && got[0] != '$' && got[0] != '\n' {
			t.Fatalf("unexpected byte %q while waiting for the soft reset", got)
		}
		if time.Now().After(deadline) {
			t.Fatal("no soft reset arrived")
		}
	}
	waitFor(t, func() bool { return strings.Contains(bridge.Status().LastIntervention, "soft reset") }, "the reset to be recorded")
}

func TestDisconnectActionCanBeTurnedOff(t *testing.T) {
	controller, address, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectNone
	})
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(bridge, StateClientConnected)
	tellMachine(t, controller, bridge, "<Run|MPos:1.000,1.000,0.000|FS:600,255>")
	client.Close()
	expectNothingWasInjected(t, controller, bridge, address)
}

// The proxy keeps its own copies of the disconnect actions so it does not
// depend on the configuration package. This is the guard against the two
// drifting apart.
func TestDisconnectActionsMatchTheConfiguration(t *testing.T) {
	for _, pair := range [][2]string{
		{DisconnectNone, config.DisconnectNone},
		{DisconnectHold, config.DisconnectHold},
		{DisconnectReset, config.DisconnectReset},
	} {
		if pair[0] != pair[1] {
			t.Errorf("proxy says %q, configuration says %q", pair[0], pair[1])
		}
	}
}

func TestControllerSilenceIsReportedNotActedOn(t *testing.T) {
	controller, address, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectHold
		c.SilenceAfter = time.Second
	})
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	waitForState(bridge, StateClientConnected)
	tellMachine(t, controller, bridge, "<Run|MPos:1.000,1.000,0.000|FS:600,255>")

	if bridge.Status().ControllerSilent {
		t.Fatal("reported silent immediately after a report")
	}
	waitFor(t, func() bool { return bridge.Status().ControllerSilent }, "the silence to be noticed")

	// Noticing is all it does. GRBL only speaks when spoken to, so a quiet
	// link is not evidence of a fault, and holding the machine on that
	// suspicion would interrupt good work on a guess.
	if bridge.Status().LastIntervention != "" {
		t.Errorf("the bridge acted on silence: %q", bridge.Status().LastIntervention)
	}
	if _, err := client.Write([]byte("G0 X1\n")); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, controller, 6); string(got) != "G0 X1\n" {
		t.Fatalf("controller received %q; the stream was disturbed", got)
	}
}

func TestSilenceIsReportedWithNobodyAttached(t *testing.T) {
	// This used to need a client, on the reasoning that GRBL only speaks when
	// spoken to, so a quiet controller with nobody connected meant nothing.
	// The appliance now does the asking itself whenever nobody else is, which
	// turns that silence into an answer: the controller is not responding.
	controller, address, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectNone
		c.SilenceAfter = 500 * time.Millisecond
		c.IdlePoll = 50 * time.Millisecond
	})
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(bridge, StateClientConnected)
	tellMachine(t, controller, bridge, "<Idle|MPos:0.000,0.000,0.000>")
	client.Close()
	waitForState(bridge, StateListening)

	waitFor(t, func() bool { return bridge.Status().ControllerSilent },
		"a controller that stopped answering the appliance's own polling to be reported")
}

func TestAControllerThatNeverAnswersIsReported(t *testing.T) {
	// A wrong device, a wrong baud rate, or a dead board. Nothing ever arrives,
	// so there is no "last report" to have gone stale - which is exactly how
	// this used to slip through as merely quiet.
	_, _, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectNone
		c.SilenceAfter = 300 * time.Millisecond
		c.IdlePoll = 50 * time.Millisecond
	})
	waitFor(t, func() bool { return bridge.Status().ControllerSilent },
		"a controller that has never said anything to be reported")
}

func TestDeviceIsPickedUpWhenItComesBack(t *testing.T) {
	// A pseudo-terminal cannot be unplugged and plugged back in, but a symlink
	// can be re-pointed, and that is what /dev/ttyUSB0 amounts to from the
	// bridge's side: a name that may resolve to different hardware over time.
	first, firstPath, err := openPTY()
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	link := filepath.Join(t.TempDir(), "ttyUSB-test")
	if err := os.Symlink(firstPath, link); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	bridge := New(Config{
		Device: link, Baudrate: 115200, Port: port,
		DeviceRetry: 20 * time.Millisecond, OnDisconnect: DisconnectNone,
	}, nil)
	done := make(chan struct{})
	ready := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if err := bridge.Run(done, ready); err != nil {
			t.Errorf("bridge stopped: %v", err)
		}
	}()
	<-ready
	t.Cleanup(func() { close(done); <-finished })

	waitFor(t, func() bool { return bridge.Status().Device != "" }, "the first device to be opened")
	if _, err := first.Write([]byte("<Run|MPos:9.000,9.000,0.000>\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return bridge.Status().Machine.State == "Run" }, "a reading from the first device")

	// Unplugged.
	first.Close()
	if state := waitForState(bridge, StateWaitingForDevice); state != StateWaitingForDevice {
		t.Fatalf("state = %s, want WAITING_FOR_DEVICE", state)
	}

	// Plugged back in - a different tty behind the same name.
	second, secondPath, err := openPTY()
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secondPath, link); err != nil {
		t.Fatal(err)
	}

	// Nobody restarts anything; the bridge finds it on its own.
	if state := waitForState(bridge, StateListening); state != StateListening {
		t.Fatalf("state = %s, want LISTENING after the device returned", state)
	}
	// And the reading describes the controller that is there now, not the one
	// that was unplugged.
	if machine := bridge.Status().Machine; machine.State != "" || machine.HasPosition {
		t.Errorf("stale reading survived the swap: %+v", machine)
	}

	client, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("$H\n")); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, second, 3); string(got) != "$H\n" {
		t.Fatalf("the new device received %q", got)
	}
}

func TestShutdownDuringStartupDoesNotHang(t *testing.T) {
	// Stopping the daemon before the serial port has been published leaves the
	// shutdown with nothing to close, and the device reader parked in a read
	// that nothing will ever interrupt. On the appliance that is an
	// rc-service stop that never returns.
	//
	// Run reports itself ready as soon as it is listening, which is before the
	// goroutine that opens the port has necessarily been scheduled at all, so
	// anything that stops the bridge immediately afterwards can take this
	// path. Observed once as a hung test; not reproducible on demand, so this
	// samples the shutdown rather than pinning the window. The guard that
	// closes it is in openPort, with the reasoning beside it.
	for attempt := 0; attempt < 5; attempt++ {
		controller, devicePath, err := openPTY()
		if err != nil {
			t.Skipf("no pseudo-terminal available: %v", err)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		listener.Close()

		bridge := New(Config{
			Device: devicePath, Baudrate: 115200, Port: port,
			DeviceRetry: time.Millisecond, IdlePoll: time.Hour, BeamGrace: time.Hour,
		}, nil)
		done := make(chan struct{})
		ready := make(chan struct{})
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			_ = bridge.Run(done, ready)
		}()
		<-ready
		close(done)

		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatalf("attempt %d: the bridge did not stop", attempt)
		}
		controller.Close()
	}
}
