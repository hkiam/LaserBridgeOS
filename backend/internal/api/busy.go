package api

import (
	"net/http"
	"strconv"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
	"github.com/laserbridgeos/laserbridgeos/backend/internal/grbl"
	"github.com/laserbridgeos/laserbridgeos/backend/internal/proxy"
)

// The appliance now knows when the machine is cutting, which means it can stop
// itself from interrupting it.
//
// Three things in this interface reach through to a running job: saving GRBL
// settings restarts the bridge, installing an update rewrites the root slot,
// and rebooting does the obvious. Before the bridge could read the stream,
// none of them could have known better. Now they can, so they ask first.
//
// This is a guard against a slip, not a permission system. Anyone who means it
// passes force=true, and the answer says so.

// machineBusy reports whether the machine is moving, and in words that can be
// shown to whoever asked.
//
// With ser2net in charge, or the daemon not answering, the honest answer is
// that we do not know - and not knowing must not block the operator from
// updating their appliance. Refusing only when there is positive evidence of a
// running job is the behaviour that fails in the right direction.
func (s *Server) machineBusy() (bool, string) {
	cfg, err := s.Store.Load()
	if err != nil || cfg.GRBL.Backend != config.BackendLaserbridged {
		return false, ""
	}
	status, err := proxy.ReadStatus(s.bridgeSocket())
	if err != nil {
		return false, ""
	}
	where := ""
	if status.Machine.HasPosition {
		where = formatPosition(status.Machine.Position)
	}
	if status.Machine.State.Moving() {
		reason := describe(status.Machine.State)
		if where != "" {
			reason += " at " + where
		}
		return true, reason
	}
	// A machine that is not moving this instant is not a machine with nothing
	// in front of it. A pierce, a pause, somebody changing the material - the
	// old question let all three through, so the guard was open at exactly the
	// moments it was written for. The bridge keeps a job open until the machine
	// has been still for a while with nobody sending.
	if status.Job.Running {
		reason := "a job is running"
		if status.Job.Lines > 0 {
			reason += " (" + strconv.FormatUint(status.Job.Lines, 10) + " lines so far)"
		}
		if where != "" {
			reason += ", the machine standing at " + where
		}
		return true, reason
	}
	return false, ""
}

// describe puts GRBL's state into a sentence. Lowercasing the word GRBL uses
// gives "the machine is run", which is not English.
func describe(state grbl.MachineState) string {
	switch state {
	case grbl.StateRun:
		return "the machine is cutting"
	case grbl.StateJog:
		return "the machine is jogging"
	case grbl.StateHome:
		return "the machine is homing"
	case grbl.StateHold:
		return "the machine is holding a job"
	case grbl.StateDoor:
		return "the machine is paused with the door open"
	default:
		// Something this appliance has not heard of, which is exactly when
		// quoting the controller verbatim is the honest thing to do.
		return "the machine reports " + string(state)
	}
}

// refuseWhileBusy answers 409 and returns true when the request should not go
// ahead. force=true in the query overrides it.
func (s *Server) refuseWhileBusy(w http.ResponseWriter, r *http.Request, what string) bool {
	if r.URL.Query().Get("force") == "true" {
		return false
	}
	busy, reason := s.machineBusy()
	if !busy {
		return false
	}
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":  what + " while " + reason + ". Repeat with force=true if that is what you want.",
		"busy":   true,
		"reason": reason,
	})
	return true
}

func formatPosition(p grbl.Position) string {
	return "X " + strconv.FormatFloat(p.X, 'f', -1, 64) + " Y " + strconv.FormatFloat(p.Y, 'f', -1, 64)
}
