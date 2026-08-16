package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// StatusSocket answers questions about the bridge over a Unix socket.
//
// A socket rather than a port: the web backend is the only caller, it runs on
// the same machine, and file permissions are a simpler access rule than
// anything that would have to be invented for TCP. It also keeps the promise
// that nothing but this daemon touches the serial port - the backend asks
// here instead of opening the device itself.
//
// The protocol is one line in, one JSON object out. "status" and an empty line
// return the current status, "journal [n]" the recent record of what went
// wrong, "command {json}" steers the machine; anything else is an error.
// Unknown commands are refused rather than guessed at, because this socket is
// world-readable by design and the answer to an unrecognised word should be a
// short one.
//
// "command" is the exception to that openness. Reading the status is not a
// secret and asking for it should not need doas; moving a machine is not in the
// same class, so the kernel is asked who is at the other end and anyone but
// root is refused. On this appliance that distinction is currently theoretical
// - one account, and it may become root without a password - but "the status is
// not a secret" was the entire argument for the permissions on this socket, and
// it does not stretch to steering.
type StatusSocket struct {
	path   string
	bridge *Bridge
}

func NewStatusSocket(path string, bridge *Bridge) *StatusSocket {
	return &StatusSocket{path: path, bridge: bridge}
}

// Serve listens until the context is cancelled.
func (s *StatusSocket) Serve(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	// A socket left behind by a previous run would make Listen fail with
	// "address already in use" even though nothing is listening.
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", s.path)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(s.path)

	// Readable by anyone on the appliance: the status is not a secret, and
	// the web backend does not run as this daemon's user.
	if err := os.Chmod(s.path, 0666); err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.answer(conn)
	}
}

func (s *StatusSocket) answer(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	line, _ := reader.ReadString('\n')
	encoder := json.NewEncoder(conn)

	fields := strings.Fields(line)
	command := ""
	if len(fields) > 0 {
		command = fields[0]
	}
	switch command {
	case "", "status":
		_ = encoder.Encode(s.bridge.Status())
	case "journal":
		limit := 0
		if len(fields) > 1 {
			limit, _ = strconv.Atoi(fields[1])
		}
		_ = encoder.Encode(s.bridge.Journal().Recent(limit))
	case "console":
		// A read, like the journal, and answered on the same terms: the
		// transcript is what the monitor port has always shown to anyone who
		// could reach it. Asking is also what keeps the recording alive, which
		// is why this is not simply another field of the status.
		after := uint64(0)
		if len(fields) > 1 {
			after, _ = strconv.ParseUint(fields[1], 10, 64)
		}
		_ = encoder.Encode(s.bridge.Console(after))
	case "command":
		uid, err := peerUID(conn)
		if err != nil || uid != 0 {
			_ = encoder.Encode(CommandAnswer{Error: "steering is only accepted from root"})
			return
		}
		var request CommandRequest
		payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "command"))
		if err := json.Unmarshal([]byte(payload), &request); err != nil {
			_ = encoder.Encode(CommandAnswer{Error: "invalid command: " + err.Error()})
			return
		}
		answer := CommandAnswer{}
		if err := s.bridge.Do(request); err != nil {
			answer.Error = err.Error()
		}
		_ = encoder.Encode(answer)
	default:
		_ = encoder.Encode(map[string]string{"error": "unknown command"})
	}
}

// askTimeout bounds a question to the daemon.
//
// Without one, a daemon that is running but wedged is worse than one that has
// died: the kernel accepts the connection on its behalf and the answer never
// comes. The web backend asks every couple of seconds, so a blocked read is
// not one stuck request but a growing pile of them, and the interface an
// operator would use to find out what is wrong stops responding too.
const askTimeout = 2 * time.Second

func ask(path, command string, into any, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = askTimeout
	}
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	if _, err := conn.Write([]byte(command + "\n")); err != nil {
		return err
	}
	return json.NewDecoder(conn).Decode(into)
}

// ReadStatus asks the daemon for its status. The web backend uses this rather
// than reaching for the serial port.
func ReadStatus(path string) (Status, error) {
	return ReadStatusWithin(path, askTimeout)
}

// ReadStatusWithin is ReadStatus for a caller that knows how long it is
// willing to wait - a liveness check wants a different answer to a page load.
func ReadStatusWithin(path string, timeout time.Duration) (Status, error) {
	var status Status
	err := ask(path, "status", &status, timeout)
	return status, err
}

// SendCommand asks the daemon to steer the machine.
func SendCommand(path string, request CommandRequest) (CommandAnswer, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return CommandAnswer{}, err
	}
	var answer CommandAnswer
	err = ask(path, "command "+string(payload), &answer, askTimeout)
	return answer, err
}

// ReadConsole asks the daemon what has crossed the wire since a sequence
// number, and by asking keeps the transcript being recorded.
func ReadConsole(path string, after uint64) (ConsoleView, error) {
	var view ConsoleView
	err := ask(path, "console "+strconv.FormatUint(after, 10), &view, askTimeout)
	return view, err
}

// ReadJournal asks the daemon what has gone wrong lately.
func ReadJournal(path string, limit int) ([]Event, error) {
	var events []Event
	err := ask(path, "journal "+strconv.Itoa(limit), &events, askTimeout)
	return events, err
}
