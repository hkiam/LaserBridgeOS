package proxy

import (
	"net"
	"strings"
	"testing"
	"time"
)

// The transcript is assembled on the path the bytes take, so the first thing to
// check is that it is not assembled when nobody is reading it.
func TestTheConsoleIsOnlyKeptWhileSomebodyIsReadingIt(t *testing.T) {
	controller, address, bridge := startBridgeWith(t, func(c *Config) {})
	waitFor(t, func() bool { return bridge.Status().Device != "" }, "the port to open")

	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	waitFor(t, func() bool { return bridge.Status().State == StateClientConnected }, "the client to attach")

	if _, err := client.Write([]byte("G0 X1\n")); err != nil {
		t.Fatal(err)
	}
	mustRead(t, controller, len("G0 X1\n"))
	if view := bridge.Console(0); len(view.Lines) != 0 || view.Recording {
		t.Fatalf("a line was kept for nobody: %+v", view)
	}

	// That first ask is what starts the recording, so what came before it is
	// gone and what comes after is kept.
	if _, err := client.Write([]byte("G0 X2\n")); err != nil {
		t.Fatal(err)
	}
	mustRead(t, controller, len("G0 X2\n"))
	if _, err := controller.Write([]byte("ok\n")); err != nil {
		t.Fatal(err)
	}

	var view ConsoleView
	waitFor(t, func() bool {
		view = bridge.Console(0)
		return len(view.Lines) >= 2
	}, "the transcript to fill")
	if !view.Recording {
		t.Error("the transcript is not being recorded although it is being read")
	}
	transcript := ""
	for _, line := range view.Lines {
		transcript += line.Mark + " " + line.Text + "\n"
	}
	// The marks are the monitor port's: > from the client, < from the
	// controller, * the appliance's own.
	if !strings.Contains(transcript, "> G0 X2") || !strings.Contains(transcript, "< ok") {
		t.Fatalf("transcript = %q", transcript)
	}
	if strings.Contains(transcript, "G0 X1") {
		t.Errorf("a line from before anybody was reading was kept: %q", transcript)
	}

	// Asking again with the last sequence number returns only what is new.
	next := view.Next
	if again := bridge.Console(next); len(again.Lines) != 0 {
		t.Errorf("the same lines were handed out twice: %+v", again.Lines)
	}
}

// A reader that comes back after the ring has turned over is told so, rather
// than being handed a transcript with a hole in it.
func TestTheConsoleSaysWhenItLostLines(t *testing.T) {
	ring := newConsole()
	now := time.Unix(1700000000, 0)
	ring.since(0, now)
	for i := 0; i < consoleRing+10; i++ {
		ring.add('>', "G0 X1", now)
	}
	view := ring.since(5, now)
	if view.Missed == 0 {
		t.Fatal("the gap was not reported")
	}
	if len(view.Lines) != consoleRing {
		t.Fatalf("kept %d lines, want %d", len(view.Lines), consoleRing)
	}
	if view.Lines[0].Seq <= 5 {
		t.Fatalf("first line handed out is %d, which the reader had already seen", view.Lines[0].Seq)
	}
}
