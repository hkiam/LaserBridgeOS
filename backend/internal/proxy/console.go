package proxy

import (
	"sync"
	"time"
)

// The console is the monitor port, for people who do not have a terminal.
//
// Everything a monitor client sees has been available for a while, over TCP, to
// whoever knows to run "nc laserbridge 23000". That is the wrong bar for the
// one question an operator asks most often - "did it get my command, and what
// did it say back?" - so the same marked transcript is kept here in a ring the
// web interface can poll.
//
// It is off unless somebody is looking. The transcript is assembled on the path
// between the client and the machine, in the same goroutines that carry the
// bytes, and a ring that filled during every job would be work done on that path
// for nobody's benefit. So a reader holds a lease, the same shape as the one
// that holds the aiming beam on: the page renews it while it is open, and a
// browser tab that goes away stops the recording within seconds.
//
// Nothing here can reach the machine. The console records what crossed the wire;
// sending is a separate path with its own guards, in control.go.
type ConsoleLine struct {
	// Seq numbers the lines so a reader can ask for what it has not seen. It
	// counts from one and never restarts while the daemon runs.
	Seq uint64 `json:"seq"`
	// Mark is ">" for the client's bytes, "<" for the controller's and "*" for
	// the appliance's own - the same three marks as the monitor port.
	Mark string `json:"mark"`
	Text string `json:"text"`
	Unix int64  `json:"unix"`
}

// ConsoleView is one reader's answer: what has happened since it last asked.
type ConsoleView struct {
	Lines []ConsoleLine `json:"lines"`
	// Next is the sequence number to ask for next time.
	Next uint64 `json:"next"`
	// Missed is how many lines fell out of the ring before this reader came
	// back for them. A gap is worth saying out loud rather than presenting a
	// transcript with a hole in it.
	Missed uint64 `json:"missed"`
	// Recording says the transcript was being kept when this was asked for. It
	// is false for the first poll after a pause, which is why a reader that
	// sees no lines is not necessarily looking at a quiet machine.
	Recording bool `json:"recording"`
}

// consoleRing is how many lines are kept. A job streams faster than anybody
// reads, so this is sized for "what just happened" rather than for a record -
// the journal is the record, and it is on disk.
const consoleRing = 400

// consoleLease is how long the transcript keeps being assembled after the last
// reader asked for it. The page polls about once a second; this is long enough
// that a slow answer or a reloaded tab does not lose the thread, and short
// enough that a closed laptop stops costing the byte path anything.
const consoleLease = 15 * time.Second

type console struct {
	mu    sync.Mutex
	lines []ConsoleLine
	// first is the sequence number of lines[0], which is what makes a gap
	// detectable rather than silent.
	first uint64
	next  uint64
	until time.Time
}

func newConsole() *console {
	return &console{lines: make([]ConsoleLine, 0, consoleRing), first: 1, next: 1}
}

// recording reports whether anybody is watching. It is called for every chunk
// of bytes in both directions, so it does nothing but take a lock and compare.
func (c *console) recording(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.until.IsZero() && now.Before(c.until)
}

// add appends one marked line.
func (c *console) add(mark byte, text string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.lines) == consoleRing {
		c.lines = append(c.lines[:0], c.lines[1:]...)
		c.first++
	}
	c.lines = append(c.lines, ConsoleLine{
		Seq:  c.next,
		Mark: string(rune(mark)),
		Text: text,
		Unix: now.Unix(),
	})
	c.next++
}

// since returns everything after a sequence number and renews the lease, so
// that asking is what keeps the recording alive.
func (c *console) since(after uint64, now time.Time) ConsoleView {
	c.mu.Lock()
	defer c.mu.Unlock()
	view := ConsoleView{
		Next:      c.next - 1,
		Recording: !c.until.IsZero() && now.Before(c.until),
	}
	c.until = now.Add(consoleLease)
	if after > 0 && after+1 < c.first {
		view.Missed = c.first - after - 1
	}
	for _, line := range c.lines {
		if line.Seq > after {
			view.Lines = append(view.Lines, line)
		}
	}
	return view
}

// stop ends the recording, for a reader that is leaving deliberately.
func (c *console) stop() {
	c.mu.Lock()
	c.until = time.Time{}
	c.mu.Unlock()
}

// Console hands out what has crossed the wire since a sequence number, and
// starts the recording if it was not running.
func (b *Bridge) Console(after uint64) ConsoleView {
	return b.console.since(after, time.Now())
}
