package proxy

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestJournalKeepsTheNewestAndDropsTheOldest(t *testing.T) {
	journal := NewJournal("")
	for i := 0; i < journalDepth+50; i++ {
		journal.Add(Event{Kind: "client", Text: string(rune('a' + i%26)), Unix: int64(i + 1)})
	}
	events := journal.Recent(0)
	if len(events) != journalDepth {
		t.Fatalf("kept %d events, want %d", len(events), journalDepth)
	}
	// Newest last, oldest first, and the first 50 are gone.
	if events[0].Unix != 51 {
		t.Errorf("oldest kept event is %d, want 51", events[0].Unix)
	}
	if events[len(events)-1].Unix != journalDepth+50 {
		t.Errorf("newest event is %d", events[len(events)-1].Unix)
	}
	if got := journal.Recent(3); len(got) != 3 || got[2].Unix != journalDepth+50 {
		t.Errorf("Recent(3) = %v", got)
	}
}

func TestJournalWritesOnlyWhatIsWorthKeeping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.log")
	journal := NewJournal(path)
	journal.Add(Event{Kind: "client", Text: "connected"})
	journal.Add(Event{Kind: "alarm", Text: "ALARM:1", Code: 1})
	journal.Add(Event{Kind: "client", Text: "disconnected"})
	journal.Add(Event{Kind: "intervention", Text: "feed hold"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want the alarm and the intervention only:\n%s", len(lines), data)
	}
	var first Event
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first.Kind != "alarm" || first.Code != 1 {
		t.Errorf("first persisted event = %+v", first)
	}
	// Clients coming and going are several a minute and are useful only while
	// looking at a live page; putting them on disk buys nothing and costs
	// writes on an appliance that boots from one.
	if strings.Contains(string(data), "connected") {
		t.Error("client events reached the disk")
	}
}

func TestJournalRotatesRatherThanGrowing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.log")
	journal := NewJournal(path)
	filler := strings.Repeat("x", 1024)
	for i := 0; i < (journalMaxBytes/1024)+8; i++ {
		journal.Add(Event{Kind: "alarm", Text: filler})
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > journalMaxBytes {
		t.Errorf("current file is %d bytes, want rotation before %d", info.Size(), journalMaxBytes)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("nothing was rotated aside: %v", err)
	}
}

func TestJournalSurvivesAnUnwritablePath(t *testing.T) {
	// A record of trouble must not become trouble of its own. On a full or
	// read-only /data the history is lost; the bridge is not.
	journal := NewJournal(filepath.Join(t.TempDir(), "no-such-dir", "\x00bad", "journal.log"))
	journal.Add(Event{Kind: "alarm", Text: "ALARM:1"})
	if len(journal.Recent(0)) != 1 {
		t.Error("the event was lost from memory as well")
	}
}

func TestLineTailKeepsWhatTheClientSent(t *testing.T) {
	tail := newLineTail()
	tail.Write([]byte("G0 X0 Y0\nG1 X10 Y10 F600\n"))
	// Split across writes, the way a TCP stream actually arrives.
	tail.Write([]byte("G1 X20"))
	tail.Write([]byte(" Y20\r\n"))
	lines := tail.Lines()
	if len(lines) != 3 || lines[2] != "G1 X20 Y20" {
		t.Fatalf("lines = %q", lines)
	}

	// Real-time bytes are not part of any line and must not be glued onto one.
	tail.Write([]byte("?G1 X30\n"))
	if got := tail.Lines(); got[len(got)-1] != "G1 X30" {
		t.Errorf("last line = %q, want the status request stripped", got[len(got)-1])
	}

	// Only the last few are kept; a job is thousands of lines.
	for i := 0; i < 20; i++ {
		tail.Write([]byte("G1 X1\n"))
	}
	if got := tail.Lines(); len(got) != contextLines {
		t.Errorf("kept %d lines, want %d", len(got), contextLines)
	}
}

func TestBridgeRecordsAnErrorNextToTheLineThatCausedIt(t *testing.T) {
	// The point of the whole journal: GRBL answers "error:9" and names no
	// line, so the only way to know what it objected to is to have kept it.
	controller, address, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectNone
	})
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	waitForState(bridge, StateClientConnected)

	if _, err := client.Write([]byte("G0 X0 Y0\nG1 X10 Y-5 F600\n")); err != nil {
		t.Fatal(err)
	}
	mustRead(t, controller, len("G0 X0 Y0\nG1 X10 Y-5 F600\n"))
	if _, err := controller.Write([]byte("error:9\r\n")); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return findEvent(bridge, "error") != nil }, "the error to be recorded")
	event := findEvent(bridge, "error")
	if event.Code != 9 {
		t.Errorf("code = %d, want 9", event.Code)
	}
	if len(event.Context) == 0 || event.Context[len(event.Context)-1] != "G1 X10 Y-5 F600" {
		t.Errorf("context = %q, want the offending line last", event.Context)
	}
}

func TestBridgeRecordsAnAlarmWithWhereItHappened(t *testing.T) {
	controller, address, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectNone
	})
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	waitForState(bridge, StateClientConnected)

	tellMachine(t, controller, bridge, "<Run|MPos:42.500,17.000,0.000|FS:600,255>")
	if _, err := controller.Write([]byte("ALARM:1\r\n")); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return findEvent(bridge, "alarm") != nil }, "the alarm to be recorded")
	event := findEvent(bridge, "alarm")
	if event.Code != 1 {
		t.Errorf("code = %d, want 1", event.Code)
	}
	// Where the machine was when it tripped is the thing you want at 3am.
	if event.Position == nil || event.Position.X != 42.5 {
		t.Errorf("position = %+v, want the last known coordinates", event.Position)
	}
}

func TestBridgeRecordsAControllerRestart(t *testing.T) {
	controller, _, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectNone
	})
	if _, err := controller.Write([]byte("Grbl 1.1f ['$' for help]\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return findEvent(bridge, "reset") != nil }, "the restart to be recorded")
}

func findEvent(bridge *Bridge, kind string) *Event {
	for _, event := range bridge.Journal().Recent(0) {
		if event.Kind == kind {
			found := event
			return &found
		}
	}
	return nil
}

func TestJournalOverTheStatusSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "bridge.sock")
	bridge := New(Config{Port: 23, Device: "/dev/null"}, nil)
	bridge.Journal().Add(Event{Kind: "alarm", Text: "ALARM:1", Code: 1})

	server := NewStatusSocket(socket, bridge)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if err := server.Serve(done); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() { close(done); <-finished })

	var events []Event
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		events, err = ReadJournal(socket, 10)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(events) != 1 || events[0].Code != 1 {
		t.Fatalf("journal over the socket = %+v", events)
	}
}

func TestStatusSocketRejectsNonsense(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "bridge.sock")
	bridge := New(Config{Port: 23, Device: "/dev/null"}, nil)
	server := NewStatusSocket(socket, bridge)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = server.Serve(done)
	}()
	t.Cleanup(func() { close(done); <-finished })

	var conn net.Conn
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err = net.Dial("unix", socket); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("rm -rf\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "unknown command") {
		t.Errorf("answer = %q", line)
	}
}

func TestRepliesAreAttributedToTheRightLine(t *testing.T) {
	// GRBL replies in order, one per line it accepted, so the third error
	// belongs to the third line - not to "one of the last few".
	tail := newLineTail()
	tail.Write([]byte("G0 X1\nG0 X2\nG0 X3\n"))
	for i, want := range []string{"G0 X1", "G0 X2", "G0 X3"} {
		if got := tail.Acknowledge(); got != want {
			t.Errorf("reply %d answered for %q, want %q", i+1, got, want)
		}
	}
}

func TestNoLineIsClaimedWhenTheCountIsOff(t *testing.T) {
	// A reply for a line this bridge never saw means the controller was
	// already working when we attached. Guessing after that would be worse
	// than saying nothing.
	tail := newLineTail()
	if got := tail.Acknowledge(); got != "" {
		t.Errorf("claimed %q with nothing outstanding", got)
	}
	tail.Write([]byte("G0 X1\n"))
	if got := tail.Acknowledge(); got != "" {
		t.Errorf("claimed %q while the count was known to be off", got)
	}

	// A new client starts the accounting over.
	tail.Forget()
	tail.Write([]byte("G0 X2\n"))
	if got := tail.Acknowledge(); got != "G0 X2" {
		t.Errorf("after starting over, claimed %q, want G0 X2", got)
	}
}

func TestBridgeNamesTheOffendingLineExactly(t *testing.T) {
	controller, address, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectNone
	})
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	waitForState(bridge, StateClientConnected)

	// Three lines out in one burst, the way a sender with a full window does.
	lines := "G0 X112.4 Y68.02\nM3 S255\nG1 X118 F900\n"
	if _, err := client.Write([]byte(lines)); err != nil {
		t.Fatal(err)
	}
	mustRead(t, controller, len(lines))

	// The controller accepts the first two and refuses the third.
	if _, err := controller.Write([]byte("ok\r\nok\r\nerror:9\r\n")); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return findEvent(bridge, "error") != nil }, "the error to be recorded")
	event := findEvent(bridge, "error")
	if event.Line != "G1 X118 F900" {
		t.Errorf("the error was attributed to %q, want G1 X118 F900", event.Line)
	}
	if len(event.Context) != 3 {
		t.Errorf("context = %q, want the surrounding lines as well", event.Context)
	}
}

func TestOkIsCountedButNotRecorded(t *testing.T) {
	// A job is thousands of these. They must not fill the record.
	controller, address, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectNone
	})
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
	time.Sleep(100 * time.Millisecond)
	if event := findEvent(bridge, "ok"); event != nil {
		t.Errorf("an ok reached the record: %+v", event)
	}
}

func TestAttributionSurvivesAFastController(t *testing.T) {
	// A client that sends without waiting, and a controller that answers while
	// it is still sending. This is the case that decides how large the queue of
	// outstanding lines has to be: every line the kernel has accepted counts as
	// sent, and its buffer holds far more than GRBL's own. At 128 the queue
	// overflowed here and attribution was given up on for the whole job.
	//
	// What this does NOT pin is the ordering in readFromClient, where a line
	// is recorded before being written rather than after. That is a
	// correctness argument about two goroutines with nothing ordering them,
	// and no burst size here reproduces it - the round trip is never fast
	// enough. The reason lives beside the code; this test covers the
	// accounting.
	controller, address, bridge := startBridgeWith(t, func(c *Config) {
		c.OnDisconnect = DisconnectNone
	})
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	waitForState(bridge, StateClientConnected)

	const lines = 600
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		buffer := make([]byte, 4096)
		seen := 0
		for seen < lines {
			n, err := controller.Read(buffer)
			if err != nil {
				return
			}
			for _, b := range buffer[:n] {
				if b != '\n' {
					continue
				}
				seen++
				if seen == lines {
					_, _ = controller.Write([]byte("error:9\r\n"))
				} else {
					_, _ = controller.Write([]byte("ok\r\n"))
				}
			}
		}
	}()

	var burst []byte
	for i := 0; i < lines; i++ {
		burst = append(burst, []byte("G1 X"+strconv.Itoa(i)+" F600\n")...)
	}
	go func() {
		// The client keeps sending while replies come back, as a real one does.
		_, _ = client.Write(burst)
	}()
	<-answered

	waitFor(t, func() bool { return findEvent(bridge, "error") != nil }, "the error to be recorded")
	want := "G1 X" + strconv.Itoa(lines-1) + " F600"
	if event := findEvent(bridge, "error"); event.Line != want {
		t.Errorf("attributed to %q, want %q", event.Line, want)
	}
}
