package proxy

import (
	"context"
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
func (b *Bridge) watch(ctx context.Context) {
	tick := b.config.IdlePoll / 2
	if tick < 100*time.Millisecond {
		tick = 100 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	silenceReported := false
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
			trigger = TriggerBeamStationary
			why = "the laser was on and the machine had not moved for " + b.config.StationaryBeam.String()
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
