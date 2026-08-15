package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/grbl"
)

// A job that fails at three in the morning leaves an operator with a stopped
// machine and no idea why. ser2net can offer nothing here - it never knew what
// it was carrying. This bridge does, so it keeps a record.
//
// What is recorded is what cannot be reconstructed afterwards: alarms and
// their codes, the G-code line an error refers to, the controller resetting,
// clients arriving and leaving, the adapter disappearing, and anything the
// bridge did on its own accord. Status reports are not recorded. There are
// several per second during a job and they would bury the three lines that
// matter.
const (
	// journalDepth is how many events are kept in memory to hand out.
	journalDepth = 200
	// journalMaxBytes bounds the file on /data. Two of these exist at most,
	// the current one and one rotation, so the worst case is twice this.
	journalMaxBytes = 256 << 10
	// contextLines is how much of what the client sent is kept, so an
	// "error:9" can be shown next to the line that caused it.
	contextLines = 4
)

// Event is one thing worth remembering.
type Event struct {
	Unix int64  `json:"unix"`
	Kind string `json:"kind"`
	Text string `json:"text"`
	Code int    `json:"code,omitempty"`
	// State and Position are the machine as it was when this happened.
	State    grbl.MachineState `json:"state,omitempty"`
	Position *grbl.Position    `json:"position,omitempty"`
	// Context is the last few lines the client sent. GRBL's error and alarm
	// replies name no line number, so without this an "error:9" is a riddle.
	Context []string `json:"context,omitempty"`
	// Line is the one line a reply answered, when the bridge has seen enough
	// of the conversation to say so. Empty when it cannot be sure.
	Line string `json:"line,omitempty"`
	// Repeats is how many further identical events followed this one. An error
	// storm is one line with a count, not a hundred lines.
	Repeats int `json:"repeats,omitempty"`
	// UptimeSeconds is how long the appliance had been running. The wall clock
	// depends on an RTC battery and a network that may not be there; this does
	// not, so the order of events survives a clock that is simply wrong.
	UptimeSeconds int64 `json:"uptime_seconds"`
}

// Journal keeps the recent past in memory and the notable part of it on disk.
//
// Writing to disk happens on its own goroutine, and that is not an
// optimisation. Add is called from the goroutine that drains the serial port,
// by way of the observer: a controller rejecting every line of a bad job -
// wrong firmware, wrong dialect - produces hundreds of events a second, and a
// file opened, written and closed for each of them would stall the reader and
// wear the disk. The record of trouble must not become trouble of its own, and
// it is not enough for it to survive write errors; it has to survive its own
// volume.
type Journal struct {
	mu    sync.Mutex
	ring  []Event
	first int
	count int

	// path is where notable events are appended; empty keeps everything in
	// memory, which is what the tests and a read-only appliance want.
	path      string
	now       func() time.Time
	writes    chan Event
	stopped   chan struct{}
	closeOnce sync.Once
	// size is the current file's length, kept by the writer goroutine alone so
	// that rotation needs no stat per line.
	size int64
	// dropped counts events that arrived faster than the disk took them. It is
	// reported rather than waited for.
	dropped atomic.Uint64
}

// journalQueue is how many events may be waiting to be written. Deep enough to
// absorb a burst, shallow enough that the memory is irrelevant, and bounded
// because the alternative to dropping is blocking the serial port.
const journalQueue = 256

func NewJournal(path string) *Journal {
	j := &Journal{ring: make([]Event, journalDepth), path: path, now: time.Now}
	if path != "" {
		j.writes = make(chan Event, journalQueue)
		j.stopped = make(chan struct{})
		go j.writeLoop()
	}
	return j
}

// Close stops the writer and waits for what is already queued. Calling it
// twice is harmless: a shutdown path that runs once in production runs from
// several places in tests, and a panic there would be about nothing.
func (j *Journal) Close() {
	if j.writes == nil {
		return
	}
	j.closeOnce.Do(func() { close(j.writes) })
	<-j.stopped
}

// writeLoop batches whatever has piled up into one open-write-close, and
// collapses repeats. An error storm is a hundred copies of one line, and a
// record that says so once with a count is more use than a hundred entries.
func (j *Journal) writeLoop() {
	defer close(j.stopped)
	var batch []Event
	for {
		event, ok := <-j.writes
		if !ok {
			return
		}
		batch = append(batch[:0], event)

		// Take whatever else is already queued, without waiting for more.
		for draining := true; draining; {
			select {
			case next, more := <-j.writes:
				if !more {
					// Closed mid-batch: write what is in hand and stop.
					j.appendBatch(collapse(batch))
					return
				}
				batch = append(batch, next)
			default:
				draining = false
			}
		}
		j.appendBatch(collapse(batch))
	}
}

// collapse turns consecutive identical events into one with a count.
func collapse(batch []Event) []Event {
	out := batch[:0:0]
	for _, event := range batch {
		if n := len(out); n > 0 && sameEvent(out[n-1], event) {
			out[n-1].Repeats++
			continue
		}
		out = append(out, event)
	}
	return out
}

func sameEvent(a, b Event) bool {
	return a.Kind == b.Kind && a.Text == b.Text && a.Code == b.Code && a.Line == b.Line
}

// worthKeeping decides what survives a reboot. Client comings and goings are
// useful while looking at a live page and not worth the write amplification of
// putting every one of them on the disk.
func worthKeeping(kind string) bool {
	switch kind {
	case "alarm", "error", "reset", "intervention", "device", "fault":
		return true
	default:
		return false
	}
}

func (j *Journal) Add(event Event) {
	if event.Unix == 0 {
		event.Unix = j.now().Unix()
	}
	if event.UptimeSeconds == 0 {
		event.UptimeSeconds = int64(time.Since(started).Seconds())
	}
	j.mu.Lock()
	index := (j.first + j.count) % len(j.ring)
	j.ring[index] = event
	if j.count < len(j.ring) {
		j.count++
	} else {
		j.first = (j.first + 1) % len(j.ring)
	}
	j.mu.Unlock()

	if j.writes == nil || !worthKeeping(event.Kind) {
		return
	}
	select {
	case j.writes <- event:
	default:
		// The disk is slower than the trouble. Keeping the appliance running
		// matters more than keeping every line of the story.
		j.dropped.Add(1)
	}
}

// Dropped is how many events never reached the disk because they arrived
// faster than it could take them.
func (j *Journal) Dropped() uint64 { return j.dropped.Load() }

// started is when this process began, which is what uptime is measured from.
var started = time.Now()

// Recent returns the newest events last, which is the order they are read in.
func (j *Journal) Recent(limit int) []Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	if limit <= 0 || limit > j.count {
		limit = j.count
	}
	events := make([]Event, 0, limit)
	for i := j.count - limit; i < j.count; i++ {
		events = append(events, j.ring[(j.first+i)%len(j.ring)])
	}
	return events
}

// append writes one line of JSON. Failures are silent on purpose: this is a
// record of trouble, and it must not become trouble of its own by taking down
// a bridge that is otherwise working. A full or read-only /data costs the
// history, not the machine.
// appendBatch writes a batch with one open and one write, rotating in the
// middle if the batch is what tips the file over the limit. The size is
// tracked rather than stat'ed per event: this runs on its own goroutine, but a
// syscall per line was the thing being fixed.
func (j *Journal) appendBatch(events []Event) {
	if len(events) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(j.path), 0755); err != nil {
		return
	}
	if j.size == 0 {
		if info, err := os.Stat(j.path); err == nil {
			j.size = info.Size()
		} else if !os.IsNotExist(err) {
			return
		}
	}

	var out []byte
	flush := func() {
		if len(out) == 0 {
			return
		}
		file, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			out = nil
			return
		}
		if n, err := file.Write(out); err == nil {
			j.size += int64(n)
		}
		_ = file.Close()
		out = nil
	}

	for _, event := range events {
		line, err := json.Marshal(event)
		if err != nil {
			continue
		}
		line = append(line, '\n')
		if j.size+int64(len(out)+len(line)) > journalMaxBytes {
			flush()
			if err := os.Rename(j.path, j.path+".1"); err != nil && !os.IsNotExist(err) {
				return
			}
			j.size = 0
		}
		out = append(out, line...)
	}
	flush()
}

// lineTail remembers what the client sent, so GRBL's replies can be attributed.
//
// GRBL answers "error:9" and nothing else - no line number, no echo - but it
// answers in order: exactly one "ok" or "error:" per line it accepted. So the
// line a reply refers to is the oldest one not yet answered for, and keeping
// a queue of them turns "error:9 somewhere around here" into "error:9 on this
// line".
//
// The queue can only be trusted while this bridge has seen every line and
// every reply. It has not, if a previous client left work in the controller's
// buffer, so a reply arriving with nothing outstanding means the count is off
// and no line is claimed until a new client starts the accounting over. The
// last few lines are kept separately as context regardless, because a
// less-precise answer is still better than none.
type lineTail struct {
	mu      sync.Mutex
	lines   []string
	pending []string
	// desynced is set once a reply arrives for a line this bridge never saw.
	desynced bool
	partial  strings.Builder
}

func newLineTail() *lineTail {
	return &lineTail{lines: make([]string, 0, contextLines)}
}

// maxPending bounds the queue.
//
// How many lines can legitimately be outstanding is not set by GRBL's 128-byte
// receive buffer but by the kernel's write buffer in front of the serial port,
// which is several kilobytes: a client that sends without waiting fills that,
// and every line in it has been sent as far as this bridge can tell. At the
// shortest realistic line length that is on the order of a thousand. Set well
// above it, because the cost of being too small is silently giving up on
// attribution for a whole job, and the cost of being too large is a few tens
// of kilobytes.
const maxPending = 4096

// Acknowledge consumes one reply and returns the line it answered, if that can
// be said with confidence.
func (l *lineTail) Acknowledge() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) == 0 {
		// A reply for a line this bridge never sent: the controller was
		// already working when we attached. Nothing can be attributed until
		// the accounting starts fresh.
		l.desynced = true
		return ""
	}
	line := l.pending[0]
	l.pending = l.pending[1:]
	if l.desynced {
		return ""
	}
	return line
}

// Forget starts the accounting over, for a new client or after a reset - both
// leave the controller with a queue this bridge cannot account for.
func (l *lineTail) Forget() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending = nil
	l.desynced = false
	l.partial.Reset()
}

func (l *lineTail) Write(data []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, b := range data {
		switch b {
		case '\n', '\r':
			if text := strings.TrimSpace(l.partial.String()); text != "" {
				l.lines = append(l.lines, text)
				if len(l.lines) > contextLines {
					l.lines = l.lines[1:]
				}
				l.pending = append(l.pending, text)
				if len(l.pending) > maxPending {
					l.pending = l.pending[1:]
					l.desynced = true
				}
			}
			l.partial.Reset()
		default:
			// Real-time bytes are not part of any line and would otherwise
			// appear glued to the front of the next one.
			if b == feedHold || b == statusReq || b == softReset || b == '~' {
				continue
			}
			if l.partial.Len() < maxContextLine {
				l.partial.WriteByte(b)
			}
		}
	}
}

const maxContextLine = 256

func (l *lineTail) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.lines) == 0 {
		return nil
	}
	return append([]string(nil), l.lines...)
}
