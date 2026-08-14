package proxy

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// startBridge runs a bridge against a pseudo-terminal and returns the
// controller side of it, the address to connect to, and a stop function.
func startBridge(t *testing.T, kickOldClient bool) (controller *os.File, address string, bridge *Bridge) {
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

	bridge = New(Config{
		Device:        devicePath,
		Baudrate:      115200,
		Port:          port,
		KickOldClient: kickOldClient,
		DeviceRetry:   20 * time.Millisecond,
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
