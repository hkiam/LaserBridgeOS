package api

import (
	"net/http"

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

func (s *Server) bridgeSocket() string {
	if s.BridgeSocket != "" {
		return s.BridgeSocket
	}
	return "/run/laserbridge/laserbridged.sock"
}
