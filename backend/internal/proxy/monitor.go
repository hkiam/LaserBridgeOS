package proxy

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The monitor port is a second TCP port that shows the traffic and accepts
// none.
//
// Diagnosing a GRBL problem usually means watching what LightBurn and the
// controller actually say to each other, and the only way to do that until now
// was to take the port away from LightBurn - which changes the situation you
// were trying to observe. Here you can watch a real job run.
//
// Everything about it is arranged so that it cannot affect the machine.
// Whatever a monitor client sends is read and discarded, never forwarded. A
// monitor client that stops reading is dropped rather than waited for. And it
// is off unless a port is configured, because a port nobody asked for is a
// port nobody is watching.
type monitors struct {
	mu   sync.Mutex
	subs map[net.Conn]struct{}
	// writeTimeout is how long a monitor may take a write before it is
	// considered gone.
	writeTimeout time.Duration
	// towards and from hold the line each direction is part-way through, so
	// the transcript is made of lines rather than of network chunks.
	towards strings.Builder
	from    strings.Builder
}

func newMonitors(writeTimeout time.Duration) *monitors {
	return &monitors{subs: map[net.Conn]struct{}{}, writeTimeout: writeTimeout}
}

func (m *monitors) add(conn net.Conn) {
	m.mu.Lock()
	m.subs[conn] = struct{}{}
	m.mu.Unlock()
}

func (m *monitors) remove(conn net.Conn) {
	m.mu.Lock()
	delete(m.subs, conn)
	m.mu.Unlock()
	_ = conn.Close()
}

func (m *monitors) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.subs)
}

// send copies bytes to every watcher. Callers are on the path between the
// machine and the client, so nothing here may block for long.
//
// The transcript is assembled into lines rather than passed through in
// whatever chunks the network and the serial port happened to produce. Those
// chunk boundaries have nothing to do with the conversation, and a transcript
// that splits "FS:600,255" across two entries is not one anybody can read.
func (m *monitors) send(prefix byte, data []byte) {
	m.mu.Lock()
	if len(m.subs) == 0 {
		// Still drop whatever was half-assembled: it belongs to a conversation
		// nobody was watching.
		m.towards.Reset()
		m.from.Reset()
		m.mu.Unlock()
		return
	}

	var out []byte
	// The bridge's own commands share the direction of the client's, so they
	// share its buffer; only the mark differs.
	partial, other := &m.towards, &m.from
	otherPrefix := byte('<')
	if prefix == '<' {
		partial, other = &m.from, &m.towards
		otherPrefix = '>'
	}
	// Whoever spoke last interrupts the other's unfinished line, so the order
	// on screen is the order it happened in.
	if other.Len() > 0 {
		out = appendLine(out, otherPrefix, other.String()+" …")
		other.Reset()
	}

	for _, b := range data {
		switch {
		case b == '\n' || b == '\r':
			if partial.Len() > 0 {
				out = appendLine(out, prefix, partial.String())
				partial.Reset()
			}
		case b == feedHold || b == statusReq || b == softReset || b == '~':
			// Real-time bytes are commands in their own right, are not part of
			// any line, and are the most interesting thing in a transcript.
			if partial.Len() > 0 {
				out = appendLine(out, prefix, partial.String())
				partial.Reset()
			}
			out = appendLine(out, prefix, realtimeName(b))
		default:
			if partial.Len() < maxContextLine {
				partial.WriteByte(b)
			}
		}
	}

	conns := make([]net.Conn, 0, len(m.subs))
	for conn := range m.subs {
		conns = append(conns, conn)
	}
	timeout := m.writeTimeout
	m.mu.Unlock()

	if len(out) == 0 {
		return
	}
	for _, conn := range conns {
		_ = conn.SetWriteDeadline(time.Now().Add(timeout))
		if _, err := conn.Write(out); err != nil {
			m.remove(conn)
		}
	}
}

func appendLine(out []byte, prefix byte, text string) []byte {
	out = append(out, prefix, ' ')
	out = append(out, text...)
	return append(out, '\n')
}

func realtimeName(b byte) string {
	switch b {
	case feedHold:
		return "! (feed hold)"
	case statusReq:
		return "? (status)"
	case softReset:
		return "0x18 (soft reset)"
	default:
		return "~ (resume)"
	}
}

func (m *monitors) closeAll() {
	m.mu.Lock()
	conns := make([]net.Conn, 0, len(m.subs))
	for conn := range m.subs {
		conns = append(conns, conn)
	}
	m.subs = map[net.Conn]struct{}{}
	m.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// serveMonitor listens on the monitor port until done is closed.
func (b *Bridge) serveMonitor(ctx context.Context) {
	if b.config.MonitorPort <= 0 {
		return
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(b.config.MonitorPort)))
	if err != nil {
		// A monitor port that will not bind is worth saying out loud and worth
		// nothing else: the bridge's actual job is unaffected.
		b.setError(err)
		return
	}
	b.logf("monitor port %d open (read-only)", b.config.MonitorPort)
	go func() {
		<-ctx.Done()
		listener.Close()
		b.monitors.closeAll()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		b.monitors.add(conn)
		b.logf("monitor %s attached", conn.RemoteAddr())
		go func() {
			defer func() {
				b.monitors.remove(conn)
				b.logf("monitor %s detached", conn.RemoteAddr())
			}()
			_, _ = conn.Write([]byte("# LaserBridgeOS monitor: > client to controller, < controller to client, * the bridge itself. Input is ignored.\n"))
			// Read and throw away. A watcher that types must not be able to
			// steer, and reading is also how a closed connection is noticed.
			buffer := make([]byte, 256)
			for {
				if _, err := conn.Read(buffer); err != nil {
					return
				}
			}
		}()
	}
}
