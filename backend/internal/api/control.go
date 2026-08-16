package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
	"github.com/laserbridgeos/laserbridgeos/backend/internal/proxy"
)

// Steering the machine from the web interface.
//
// The interface has no login (ADR 0014), which was defensible while the worst
// it could do was restart a service: anyone who can reach port 80 is already on
// the workshop network and can pull the plug. These endpoints move a machine and
// can switch a laser on, which is a different class of thing, and they are
// bounded accordingly - by what they will not do rather than by who is asking.
//
//   - Nothing at all unless laserbridged owns the port. ser2net has no idea what
//     it is carrying and no way to ask.
//   - Nothing while a client is connected, except Stop. Two applications
//     steering one laser is the failure the appliance exists to prevent.
//   - The aiming beam is a lease that has to be renewed every second, so a page
//     that goes away takes the beam with it.
//
// Same-origin and CSRF are enforced by the mutation wrapper, as everywhere else.
// holderOf names the browser session that is asking, without handing its
// secret to anybody.
//
// The CSRF cookie already identifies a session; hashing it gives something
// stable enough to hold a lease and to name in the record, while the token
// itself never leaves this process. Without a cookie there is no session and no
// lease can be held - which the mutation wrapper has already refused by then.
func holderOf(r *http.Request) string {
	cookie, err := r.Cookie(csrfCookie)
	if err != nil || cookie.Value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(cookie.Value))
	return "session " + hex.EncodeToString(sum[:4])
}

type controlRequest struct {
	Axis     string   `json:"axis,omitempty"`
	Distance float64  `json:"distance,omitempty"`
	Feed     float64  `json:"feed,omitempty"`
	Percent  int      `json:"percent,omitempty"`
	X        *float64 `json:"x,omitempty"`
	Y        *float64 `json:"y,omitempty"`
	Z        *float64 `json:"z,omitempty"`
	Line     string   `json:"line,omitempty"`
}

func (s *Server) grblControl(w http.ResponseWriter, r *http.Request) {
	command := proxy.Command(r.PathValue("command"))
	cfg, err := s.Store.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cfg.GRBL.Backend != config.BackendLaserbridged {
		writeError(w, http.StatusConflict,
			"steering needs the laserbridged backend; ser2net cannot tell what it is carrying")
		return
	}

	var request controlRequest
	if r.Body != nil && r.ContentLength != 0 {
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
	}

	answer, err := proxy.SendCommand(s.bridgeSocket(), proxy.CommandRequest{
		Command:  command,
		Axis:     request.Axis,
		Distance: request.Distance,
		Feed:     request.Feed,
		X:        request.X,
		Y:        request.Y,
		Z:        request.Z,
		Percent:  request.Percent,
		Line:     request.Line,
		Holder:   holderOf(r),
	})
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "the bridge did not answer: "+err.Error())
		return
	}
	if answer.Error != "" {
		// The daemon answers in words, over a socket, so the reason arrives as
		// a string rather than as an error to unwrap. Matching on it is ugly and
		// honest; inventing a numeric protocol between two halves of the same
		// program would be neither.
		status := http.StatusUnprocessableEntity
		switch {
		case strings.Contains(answer.Error, "client is connected"):
			status = http.StatusConflict
		case strings.Contains(answer.Error, "no serial port"):
			status = http.StatusServiceUnavailable
		case strings.Contains(answer.Error, "unknown command"):
			status = http.StatusNotFound
		case strings.Contains(answer.Error, "only accepted from root"):
			status = http.StatusForbidden
		case strings.Contains(answer.Error, "somebody else is holding"):
			status = http.StatusConflict
		case strings.Contains(answer.Error, "has not answered the last"):
			status = http.StatusTooManyRequests
		}
		writeError(w, status, answer.Error)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "sent", "command": string(command)})
}
