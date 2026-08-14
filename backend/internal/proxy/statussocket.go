package proxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// StatusSocket answers questions about the bridge over a Unix socket.
//
// A socket rather than a port: the web backend is the only caller, it runs on
// the same machine, and file permissions are a simpler access rule than
// anything that would have to be invented for TCP. It also keeps the promise
// that nothing but this daemon touches the serial port - the backend asks
// here instead of opening the device itself.
//
// The protocol is one line in, one JSON object out. "status" and an empty
// line both return the current status; anything else is an error. That is
// enough for now and leaves room for the commands that Phase 9 will add
// without changing how callers connect.
type StatusSocket struct {
	path   string
	bridge *Bridge
}

func NewStatusSocket(path string, bridge *Bridge) *StatusSocket {
	return &StatusSocket{path: path, bridge: bridge}
}

// Serve listens until done is closed.
func (s *StatusSocket) Serve(done <-chan struct{}) error {
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
		<-done
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

	switch strings.TrimSpace(line) {
	case "", "status":
		_ = encoder.Encode(s.bridge.Status())
	default:
		_ = encoder.Encode(map[string]string{"error": "unknown command"})
	}
}

// ReadStatus asks the daemon for its status. The web backend uses this rather
// than reaching for the serial port.
func ReadStatus(path string) (Status, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return Status{}, err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("status\n")); err != nil {
		return Status{}, err
	}
	var status Status
	if err := json.NewDecoder(conn).Decode(&status); err != nil {
		return Status{}, err
	}
	return status, nil
}
