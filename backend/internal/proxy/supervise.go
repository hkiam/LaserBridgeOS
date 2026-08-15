package proxy

import (
	"context"
	"strconv"
	"time"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/grbl"
)

// watch is the standing supervision, as opposed to the disconnect handler's
// reaction to one particular event.
//
// The disconnect handler acts on a symptom: the client went away. The hazard
// it stands in for is different and larger - the laser is on and the machine
// is not moving - and that hazard has causes the disconnect handler cannot
// see. LightBurn can hang with its connection intact and its job half sent.
// Our own escalation can fail. A controller can sit in Hold with the output
// still live. So the invariant is watched directly, rather than one of the
// ways it can be broken.
//
// Two conditions, because they carry different risks of being wrong:
//
//   - The laser is on with nobody attached. There is no legitimate version of
//     this, so the grace is short.
//   - The laser is on and the machine has not moved. Piercing thick material
//     is a stationary burn on purpose, and so is aiming the beam by hand, so
//     the grace here has to be longer than any of those - and it is the one
//     number worth putting in the configuration.
//
// It asks the controller for a status report only when nobody else has for a
// while. During a job the client polls several times a second and the
// appliance stays silent; when the conversation stops, it takes over the
// asking. That is the one thing it injects into a client's stream, it is
// read-only, and without it the watchdog would be reading a number that
// stopped updating exactly when it started mattering.
// beamEvidenceReports is how many status reports may pass after the controller
// last mentioned its outputs before the bridge stops treating that as current.
// GRBL's widest documented gap is twenty; thirty leaves room for a fork that
// counts differently without letting a genuinely stale reading through.
const beamEvidenceReports = 30

func (b *Bridge) watch(ctx context.Context) {
	tick := b.config.IdlePoll / 2
	if tick < 100*time.Millisecond {
		tick = 100 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	silenceReported := false
	staleReported := false
	var beamOnSince, movedAt time.Time
	var lastPosition grbl.Position

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		port := b.currentPort()
		if port == nil {
			beamOnSince, movedAt = time.Time{}, time.Time{}
			continue
		}
		// Only when nobody else is asking.
		if b.observer.Quiet(b.config.IdlePoll) {
			if err := b.writeOwn(port, statusReq); err != nil {
				continue
			}
		}

		if silent := b.controllerSilent(); silent != silenceReported {
			silenceReported = silent
			if silent {
				b.logf("the controller is not answering")
				b.note("fault", "the controller stopped answering")
			}
		}

		machine := b.observer.Machine()
		now := time.Now()
		// The job is followed here because this is the one loop that sees the
		// machine at a steady rate whether or not anybody is connected.
		if note := b.jobs.observe(machine, b.lines.Load(), b.rx.Load(), now, int64(time.Since(started).Seconds())); note != "" {
			b.note("job", note)
		}
		if machine.HasPosition && (machine.Position != lastPosition || movedAt.IsZero()) {
			lastPosition, movedAt = machine.Position, now
		}
		if machine.Beam != grbl.BeamOn {
			beamOnSince = time.Time{}
			continue
		}
		if beamOnSince.IsZero() {
			beamOnSince = now
		}
		if b.config.OnDisconnect == DisconnectNone {
			continue
		}

		var trigger Trigger
		why := ""
		switch {
		case b.currentClient() == nil && now.Sub(beamOnSince) > b.config.BeamGrace:
			trigger = TriggerBeamUnattended
			why = "the laser was on with nobody connected"
		case b.config.StationaryBeam > 0 && machine.HasPosition &&
			now.Sub(movedAt) > b.config.StationaryBeam && now.Sub(beamOnSince) > b.config.StationaryBeam:
			// The evidence has a date on it, and by the time this fires the
			// oldest of it is a whole grace period old. GRBL only mentions its
			// outputs alongside Ov: - every tenth report or so - so "on for
			// twenty seconds" can rest on a single observation from twenty
			// seconds ago, and in laser mode with M4 that observation is taken
			// at an instant when the output happened to be on.
			//
			// That was tolerable while an intervention only cost a feed hold.
			// It is not now that the bridge drops the client with it: a false
			// positive ends the job outright. So the controller has to have
			// mentioned its outputs recently, otherwise this is acting on a
			// memory.
			//
			// "Recently" is counted in reports and not in seconds, and that
			// distinction was itself a bug for an afternoon. Tying it to the
			// grace period looks reasonable and is unsatisfiable: with a grace
			// of two seconds - which the configuration allows - and a block
			// every tenth report, no evidence is ever young enough and the
			// check switches itself off in silence. Reports are the unit the
			// controller actually meters this in.
			if machine.BeamUnix == 0 || machine.Reports-machine.BeamReports > beamEvidenceReports {
				if !staleReported {
					staleReported = true
					b.note("fault", "the laser was reported on and the machine has not moved, "+
						"but the controller has said nothing about its outputs since; not acting on that")
				}
				continue
			}
			staleReported = false
			trigger = TriggerBeamStationary
			why = "the laser was on and the machine had not moved for " + b.config.StationaryBeam.String() +
				" (last reported on " + now.Sub(time.Unix(machine.BeamUnix, 0)).Round(time.Second).String() + " ago, S" +
				strconv.FormatFloat(machine.Spindle, 'f', -1, 64) + ")"
		default:
			continue
		}

		// TryLock rather than Lock: if the disconnect handler is already
		// working on this, it does not need a second opinion arriving in the
		// middle of its own accounting - the spindle-stop override is a toggle.
		//
		// And when it is busy, the timers are left standing. Resetting them
		// here would restart the grace period on every blocked tick, so after
		// a long intervention the beam would get a fresh grace period before
		// anything looked at it again - which is precisely the situation where
		// it has already been on too long.
		if !b.interveneMu.TryLock() {
			continue
		}
		beamOnSince, movedAt = time.Time{}, time.Time{}
		b.logf("%s", why)
		b.note("fault", why)
		b.intervene(port, trigger, why)
		b.interveneMu.Unlock()
	}
}

// controllerSilent reports whether the controller has stopped answering, or
// never started.
//
// Never having spoken counts, once the port has been open long enough for an
// answer: with the appliance now polling whenever nobody else is, a controller
// that has said nothing at all is a wrong device, a wrong baud rate, or a dead
// board - not simply one nobody has addressed.
func (b *Bridge) controllerSilent() bool {
	b.mu.Lock()
	openedAt := b.portOpenedAt
	b.mu.Unlock()
	if openedAt.IsZero() {
		return false
	}
	if b.observer.Machine().LastReportUnix == 0 {
		return time.Since(openedAt) > b.config.SilenceAfter
	}
	return b.observer.Silent(b.config.SilenceAfter)
}
