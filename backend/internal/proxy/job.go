package proxy

import (
	"strconv"
	"sync"
	"time"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/grbl"
)

// The appliance thinks in bytes, clients and machine states. The operator
// thinks in "my forty-minute cut", and until now there was nothing in between.
//
// The gap showed up in three places at once. The guard that refuses to restart
// the bridge or install an update "while the machine is moving" is false during
// a pierce, during a pause, and while somebody changes the material - so it
// permits exactly the interruptions it exists to prevent. The record can say an
// alarm happened at 03:14 but not that it happened eleven minutes into a job.
// And after the bridge stops a machine itself, nobody can say what was lost.
//
// So the smallest useful notion of a job: it begins when the machine starts
// moving and ends when it has been still for a while with nobody sending, or
// when something ends it. No estimates, no progress, no persistence - those
// need to know what the file was, and this only knows what crossed the wire.
type Job struct {
	Running bool `json:"running"`
	// StartedUnix is the wall clock, StartedUptime the appliance's own. The
	// second one survives a clock that is simply wrong, which on a device whose
	// RTC battery may be flat is the one that can be trusted for durations.
	StartedUnix   int64 `json:"started_unix,omitempty"`
	StartedUptime int64 `json:"started_uptime_seconds,omitempty"`
	EndedUnix     int64 `json:"ended_unix,omitempty"`
	// Ended says why, in words for an operator: finished, or stopped, or the
	// client left. Empty while it runs.
	Ended string `json:"ended,omitempty"`
	// Seconds is how long it ran, taken from the uptime clock.
	Seconds int64 `json:"seconds"`
	// Lines and Bytes are what the client sent during it. Lines is the number
	// that matters - it is what a sender counts too.
	Lines uint64 `json:"lines"`
	Bytes uint64 `json:"bytes"`
	// Start and Last are where the machine was when it began and when it was
	// last seen. A job that ended somewhere unexpected is worth being able to
	// see afterwards.
	Start grbl.Position `json:"start_position"`
	Last  grbl.Position `json:"last_position"`
}

// jobQuiet is how long a machine has to stand still before the work in front of
// it counts as over. It has to be longer than a pause between two cuts of the
// same job and than the longest pierce, and shorter than the patience of
// somebody waiting to install an update.
const jobQuiet = 15 * time.Second

type jobTracker struct {
	mu   sync.Mutex
	job  Job
	rest time.Time
	// startLines and startBytes are the counters as they stood when the job
	// began; what it sent is the difference.
	startLines uint64
	startBytes uint64
}

// observe advances the job from one reading of the machine. It returns a
// sentence for the record when a job has just ended, and the empty string
// otherwise - the caller writes it, so that no lock of this package's is held
// while the journal takes its own.
func (t *jobTracker) observe(machine grbl.Machine, clientConnected bool, lines, bytes uint64, now time.Time, uptime int64) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	moving := machine.State.Moving()

	if !t.job.Running {
		// A job is a client's work. Without one there is somebody at the jog
		// pad, and MachineState.Moving() is true for Jog and Home as well as
		// Run - so without this every tap on an arrow key started a "job",
		// wrote a line into the record saying it had finished after three
		// seconds and no lines, and blocked updates and reboots for fifteen
		// seconds afterwards.
		if !moving || !clientConnected {
			return ""
		}
		t.job = Job{
			Running:       true,
			StartedUnix:   now.Unix(),
			StartedUptime: uptime,
			Start:         machine.Position,
			Last:          machine.Position,
		}
		t.startLines, t.startBytes = lines, bytes
		t.rest = time.Time{}
		return ""
	}

	t.job.Last = machine.Position
	t.job.Lines = lines - t.startLines
	t.job.Bytes = bytes - t.startBytes
	t.job.Seconds = uptime - t.job.StartedUptime
	if moving {
		t.rest = time.Time{}
		return ""
	}
	if t.rest.IsZero() {
		t.rest = now
		return ""
	}
	if now.Sub(t.rest) < jobQuiet {
		return ""
	}
	return t.endLocked("finished", now)
}

// end closes the job for a reason other than it simply running out.
func (t *jobTracker) end(reason string, now time.Time) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.endLocked(reason, now)
}

func (t *jobTracker) endLocked(reason string, now time.Time) string {
	if !t.job.Running {
		return ""
	}
	t.job.Running = false
	t.job.Ended = reason
	t.job.EndedUnix = now.Unix()
	t.rest = time.Time{}
	return "job " + reason + " after " + (time.Duration(t.job.Seconds) * time.Second).String() +
		" and " + strconv.FormatUint(t.job.Lines, 10) + " lines"
}

func (t *jobTracker) current() Job {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.job
}
