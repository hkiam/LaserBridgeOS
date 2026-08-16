package api

import (
	"net/http"
	"strconv"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
	"github.com/laserbridgeos/laserbridgeos/backend/internal/proxy"
)

// grblStatus answers with what the bridge says about itself.
//
// The web backend asks the daemon over its socket rather than opening the
// serial port, because exactly one process may hold that port and it is not
// this one. With ser2net selected there is nobody to ask, and saying so is a
// better answer than an error.
func (s *Server) grblStatus(w http.ResponseWriter, _ *http.Request) {
	cfg, err := s.Store.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cfg.GRBL.Backend != config.BackendLaserbridged {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"backend":   cfg.GRBL.Backend,
			"reason":    "the machine reading is only available with the laserbridged backend",
		})
		return
	}
	status, err := proxy.ReadStatus(s.bridgeSocket())
	if err != nil {
		// The daemon may be restarting, or may have died. Either way the web
		// interface should say the reading is unavailable rather than break.
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"backend":   cfg.GRBL.Backend,
			"reason":    err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": true,
		"backend":   cfg.GRBL.Backend,
		"bridge":    status,
	})
}

// grblJournal hands out what the bridge has recorded.
//
// A separate endpoint from the status: the status page asks every couple of
// seconds and has no business carrying two hundred events with it.
func (s *Server) grblJournal(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 1 && parsed <= 200 {
			limit = parsed
		}
	}
	cfg, err := s.Store.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cfg.GRBL.Backend != config.BackendLaserbridged {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "events": []proxy.Event{}})
		return
	}
	events, err := proxy.ReadJournal(s.bridgeSocket(), limit)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "events": []proxy.Event{}, "reason": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "events": events})
}

// grblConsole hands out the transcript: what the client and the controller
// have said to each other since the caller last asked.
//
// The polling is what keeps it being recorded, which is deliberate - see the
// note in the proxy package. A caller that stops asking stops the recording
// within seconds, and the byte path between the machine and the client goes
// back to doing nothing on anybody's behalf.
func (s *Server) grblConsole(w http.ResponseWriter, r *http.Request) {
	after := uint64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		if parsed, err := strconv.ParseUint(raw, 10, 64); err == nil {
			after = parsed
		}
	}
	cfg, err := s.Store.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cfg.GRBL.Backend != config.BackendLaserbridged {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "lines": []proxy.ConsoleLine{}})
		return
	}
	view, err := proxy.ReadConsole(s.bridgeSocket(), after)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "lines": []proxy.ConsoleLine{}, "reason": err.Error()})
		return
	}
	if view.Lines == nil {
		view.Lines = []proxy.ConsoleLine{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": true,
		"lines":     view.Lines,
		"next":      view.Next,
		"missed":    view.Missed,
		"recording": view.Recording,
	})
}

func (s *Server) bridgeSocket() string {
	if s.BridgeSocket != "" {
		return s.BridgeSocket
	}
	return "/run/laserbridge/laserbridged.sock"
}
